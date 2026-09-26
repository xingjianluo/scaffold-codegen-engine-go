# scaffold-codegen-engine-go

一个**纯本地**的声明式代码生成 / 脚手架引擎。给一份声明式生成计划（JSON），
引擎负责：

- 按依赖顺序生成多文件工程骨架；
- 循环依赖、缺失参数、路径冲突等问题**一次性全部检出并分类拒绝**，不静默跳过；
- 幂等、可复现：相同输入无论执行顺序、worker 数量如何，产物内容与 SHA-256
  摘要完全一致；
- 对"用户改过的文件 / 已存在但不受管的文件 / 计划自身的更新"给出**可区分、
  可解释**的结论，绝不覆盖无关改动；
- 有界并发调度，共享状态全部加锁；任一任务失败或运行被中断时执行
  **全量回滚**（all-or-nothing），重试后恢复到可判定、可诊断的状态。

无任何外部服务、真实凭据或网络依赖；仅使用 Go 标准库。

## 目录结构

```
internal/plan/     计划类型、静态校验、确定性拓扑排序
internal/engine/   渲染、状态清单、并发调度器、提交/回滚、分类错误
cmd/scaffold/      本地验证 CLI（validate / run / plan-example）
```

## 计划格式

```json
{
  "name": "example",
  "params": { "module": "example.com/myapp" },
  "targets": {
    "go-mod": {
      "path": "go.mod",
      "template": "module {{.params.module}}\n\ngo 1.25\n"
    },
    "main": {
      "path": "main.go",
      "template": "package main // {{deps \"go-mod\"}}",
      "deps": ["go-mod"],
      "requiredParams": ["module"]
    }
  }
}
```

字段语义：

| 字段 | 语义 |
| --- | --- |
| `name` | 计划名，仅用于报告展示。 |
| `params` | 字符串参数表，模板通过 `{{.params.KEY}}` 读取。 |
| `targets` | 目标集合，**map 的键就是目标 ID**（权威 ID）。 |
| `targets.*.path` | 相对输出根目录的产物路径。必须是 `/` 分隔、相对、已规范化（不允许 `./`、`//`、`..`、绝对路径）。 |
| `targets.*.template` | Go `text/template` 语法的模板正文。开启 `missingkey=error`，引用不存在的参数/数据会直接导致渲染失败，不会静默输出 `<no value>`。 |
| `targets.*.deps` | 依赖的目标 ID 列表。依赖保证先处理；模板可通过函数 `{{deps "ID"}}` 读取依赖的渲染结果。 |
| `targets.*.requiredParams` | 必须存在且非空的参数键。 |
| `targets.*.fail` | 调试/演练用：置 `true` 时该目标必然渲染失败，用于练习部分失败与恢复。 |

模板确定性约定：不使用任何时间、随机数、环境变量或 map 顺序；相同输入必然
得到相同字节。

## 校验与错误分类

校验不会"遇到第一个错误就停"，而是收集全部问题并按 `(code, target, message)`
排序后返回，一次解释清楚计划的所有问题。错误码：

| code | 含义 |
| --- | --- |
| `empty_id` / 空目标 | 目标 ID 为空或目标为 null。 |
| `empty_path` | 产物路径为空。 |
| `path_absolute` | 路径是绝对路径。 |
| `path_traversal` | 路径包含 `..`，会逃逸出输出根目录。 |
| `path_not_normalized` | 路径含反斜杠、`./`、`//` 等非规范形式。 |
| `path_conflict` | 两个或更多目标声明同一路径——**即使模板内容相同也拒绝**（声明冲突而非内容冲突），每个属主都会收到一条问题。 |
| `missing_dep` | 依赖了不存在的目标。 |
| `self_dep` | 目标依赖自身。 |
| `cycle` | 依赖图存在环；消息中给出完整环路，如 `a -> b -> c -> a`。 |
| `missing_param` | `requiredParams` 中的参数缺失或为空字符串。 |

校验不通过时运行**立即终止，不触碰任何文件**（包括 `.scaffold/`）。

## 处理顺序

1. 校验通过后做拓扑排序；使用 Kahn 算法并在所有"就绪"目标中始终选择
   **ID 最小**者，因此顺序与 map 遍历、worker 数量完全无关，是确定的。
2. 调度器维护就绪队列与有界 worker 池（`Options.Workers`，默认/最小 1）：
   依赖未全部终态的目标不会被派发；任一依赖失败/被跳过，则该目标记为
   `skipped` 并把跳过向下游传播。
3. 报告中的 `order` 与 `results` 始终按拓扑顺序排列；并发只影响完成时刻，
   不影响结果排列。

## 状态清单与冲突判定（幂等模型）

每次成功运行会在输出根目录原子提交 `.scaffold/manifest.json`
（临时文件 + `fsync` + `rename`，崩溃也只会留下旧版或新版，绝不会写坏），
记录每个受管路径的：目标 ID、路径、内容 SHA-256、完整内容。

处理一个目标时，渲染出期望内容后，与磁盘和清单比对，得到以下互斥结论：

| status | 判定条件 | 动作 |
| --- | --- | --- |
| `created` | 磁盘上不存在该文件 | 写入文件。 |
| `verified` | 文件受同一目标管理，磁盘内容与上次一致，且本次渲染摘要相同 | 不写入。 |
| `recreated` | 文件受同一目标管理且未被改动，但本次计划渲染内容变了 | 以新内容确定性覆盖。 |
| `modified` | 文件受同一目标管理，但**磁盘内容与清单记录不一致**（用户事后改过） | **保持文件原样**，报告解释双方 SHA-256；属于成功运行（显式结论而非失败）。 |
| `preexisting` | 文件存在，但清单中没有该路径的记录，或该路径登记在**别的目标 ID** 名下 | **保持文件原样**，不覆盖、不接管。 |
| `failed` | 渲染/写入失败，或运行被中断。 | 不提交；参与回滚决策。 |
| `skipped` | 上游依赖失败，或中断时还未开始。 | 不执行。 |

因此重复执行不会覆盖无关改动：`modified` 与 `preexisting` 是两类不同的
可解释结论；只有"文件未被触碰、且计划自身改变了期望输出"才会
`recreated`。

清单合并规则：成功提交时，本次计划涉及的目标条目整体替换（目标改了输出
路径时旧路径条目被移除），属于其他目标/计划的条目原样保留。

## 失败恢复规则

- 运行内创建的文件记录在回滚集合中（仅本次 **新建** 的文件；`recreated`
  覆盖的是受管文件，其原始内容可由旧清单追溯）。
- 任意 `failed`、`skipped`（上游失败）或 `context` 中断：
  1. 停止派发新任务，等待已在执行的任务到达终态；
  2. 删除回滚集合中的全部文件，使磁盘恢复到运行前状态；
  3. **不更新清单**（旧清单原封不动，清单写入本身是原子的）；
  4. 返回带分类的错误和尽力而为的报告（每个目标的 status、reason、摘要、
     attempt 次数都可诊断）。
- 修正计划（或恢复上下文）后重试：因为磁盘与清单已回到一致的前态，重试
  会像第一次运行一样得到确定的 `created`/`verified` 结论。
- 同一 `Engine` 实例维护内存中的 `attempt` 计数：目标**真正被处理**一次
  （包括处理后失败）计数 +1；仅被 `skipped` 不计。新进程/新 Engine 从 1
  开始。

运行错误分类（CLI 退出码）：

| kind | 含义 | 退出码 |
| --- | --- | --- |
| `invalid_plan` | 静态校验拒绝，未触碰文件。 | 2 |
| `render_error` | 模板解析/执行失败。 | 3 |
| `write_error` / `manifest_error` | 文件 IO 或清单提交失败。 | 4 |
| `interrupted` | 上下文取消 / 收到中断信号。 | 5 |
| `partial_failure` | 存在失败/跳过的兜底分类。 | 6 |

## 并发安全

- 一个 `Engine` 上的 `Run` 整体互斥（同一根目录的并发运行被串行化，先到者
  创建、其余验证，结果仍确定）。
- 单次运行内：调度共享状态由单一互斥量 + 条件变量保护；文件系统决策由
  运行级锁串行化；attempt 计数有独立互斥量。`go test -race` 干净。

## 本地验证方法

全部离线完成，不需要网络：

```bash
# 单元测试（plan + engine，覆盖循环依赖/并发/部分失败恢复/中断回滚）
go test ./...

# 竞态检测 + 重复运行
go test -race -count=5 ./...

# 静态检查与构建
go vet ./...
go build ./...

# CLI 端到端
go run ./cmd/scaffold plan-example -o /tmp/plan.json
go run ./cmd/scaffold validate -f /tmp/plan.json
go run ./cmd/scaffold run -f /tmp/plan.json -root /tmp/out -workers 4
go run ./cmd/scaffold run -f /tmp/plan.json -root /tmp/out   # 再跑一次：全部 verified
```

单测日志（`go test -v`）会打印每个场景的**输入计划**（`INPUT PLAN`）、
**处理顺序**与**最终状态**（`PROCESSING ORDER + FINAL STATE`，含每个目标的
status、SHA-256、reason、attempt）。
