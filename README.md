# scaffold-codegen-engine-go

一个**纯本地**的声明式代码生成 / 脚手架引擎。给一份声明式生成计划（JSON），
引擎负责：

- 按依赖顺序生成多文件工程骨架；
- 循环依赖、缺失参数、路径冲突等问题**一次性全部检出并分类拒绝**，不静默跳过；
- 幂等、可复现：相同输入无论执行顺序、worker 数量如何，产物内容与 SHA-256
  摘要完全一致；
- 对"用户改过的文件 / 已存在但不受管的文件 / 计划自身的更新"给出**可区分、
  可解释**的结论，绝不覆盖无关改动；
- **只预演不落盘**：`preview` 给出与真实执行逐条一致的处理顺序和每目标结论，
  全程不创建、不修改、不删除工作区里的任何文件；
- **随时可查的状态**：`status` 能在上次正常结束、报错或被中途打断之后，报告
  每个目标最后停在哪一步，并把"产物被使用者改过"与"产物已被删除"分开说明；
- **多人/多流水线安全**：基于跨进程文件锁（flock），同一工作区上后到的运行
  要么立刻得到明确的 `workspace_busy` 占用结论，要么用 `-wait-lock` 排队；
- 有界并发调度，共享状态全部加锁；任一任务失败或运行被中断时，依据运行前
  备份把**新建的文件删除、覆盖的文件还原**，使工作区字节级回到运行开始前；
  进程被硬杀导致的残留，下次运行会先从备份恢复，再执行计划（all-or-nothing），
  重试后恢复到可判定、可诊断的状态。

无任何外部服务、真实凭据或网络依赖；仅使用 Go 标准库。

## 目录结构

```
internal/plan/     计划类型、静态校验、确定性拓扑排序
internal/engine/   渲染、状态清单、并发调度器、预演、状态查询、
                   跨进程锁、运行前备份/恢复、提交/回滚、分类错误
cmd/scaffold/      本地验证 CLI（validate / preview / status / run / plan-example）
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

## 预演（dry run）

```bash
scaffold preview -f plan.json -root ./out
```

预演先做静态校验，再按真实执行的拓扑顺序渲染每个目标，对每个路径调用与
`run` **完全相同**的判定逻辑（同一份 `decide` 函数），给出逐条结论。

- 预演**完全不写盘**：不创建输出文件，也不创建 `.scaffold/`（甚至不会创建
  锁文件）；在一个从没跑过的空目录上执行预演后，目录仍然为空。
- 输出同样包含 `order`（处理顺序）和每个目标的 `status`、`sha256`、`reason`。
- 结论词汇与真实执行一致：`created` / `verified` / `recreated` / `modified` /
  `preexisting`；上游会失败时，下游同样记为 `skipped`，失败目标记为
  `failed`。含义同[状态清单与冲突判定](#状态清单与冲突判定幂等模型)一节。
- **语义边界**：预演描述的是"以当前磁盘和清单为起点、立即执行一次"的结果。
  预演与真实执行之间如果有人改了文件、删了文件，或提交了别的清单，结论自然
  会变；在没有这种介入的前提下，`preview` 与紧接着的 `run` 对每个目标给出的
  `status`、顺序和摘要逐条相同（单测 `TestPreview_ConclusionsMatchRealRun`
  覆盖了新建、原样复用、计划更新重写、人为改动、不受管文件，以及失败/跳过
  传播这几种情形）。
- 预演本身是只读的，因此不参与互斥排队；但如果此刻正有一次独占 `run` 在写
  工作区，预演会以 `workspace_busy` 拒绝，避免读到写入中途的状态而给出
  误导性结论。预演中若存在会失败的目标，会像真实执行一样返回
  `render_error` 分类错误（但仍然什么都不写）。

## 状态查询

```bash
# 只对照上次运行与磁盘
scaffold status -root ./out
# 额外对照"当前计划"，判断磁盘是否还跟计划一致
scaffold status -root ./out -f plan.json
```

`status` 只读、不改任何东西，即使上次运行异常结束也能查。报告包含：

- `phase`：上一次运行停在哪个阶段——`succeeded` / `failed` / `interrupted` /
  `running` / `rolling_back`。后两者表示进程在运行中或回滚中被硬杀
  （`stale=true`）。
- 每个目标的 `lastRun`（上次记录的结论）与 `step`（最后到达的细粒度步骤：
  `pending → rendering → rendered → decided → written → terminal`），以及当前
  磁盘状况 `disk`。
- `consistent`：磁盘是否与上次提交的清单一致；`matchesPlan`：在此之上磁盘
  是否还等于当前计划渲染出来的内容。

`disk` 把两类"使用者动过"的情况明确分开：

| disk | 含义 |
| --- | --- |
| `in_sync` | 磁盘与上次提交一致，且与当前计划渲染一致。 |
| `plan_changed` | 磁盘与上次提交一致，但当前计划会渲染出不同内容（再跑会 `recreated`）。 |
| `user_modified` | 磁盘内容与上次提交的记录不一致——**产物被使用者改过**。 |
| `deleted` | 上次提交过的产物**已经从磁盘上被删除**。 |
| `planned_absent` | 路径在当前计划里、磁盘上不存在、也从未生成过。 |
| `unexpected` | 路径上有文件，但清单里没有归当前目标所有的记录（不受管/属主不符）。 |

## 多人共用与并发占用

- 每次 `run` 开始时先在 `.scaffold/lock` 上获取一个**跨进程**的排他文件锁
  （POSIX `flock`）。同一工作区上，无论来自多个 Engine 实例、多个进程还是
  多条 CI 流水线，同时只有一次运行能写盘。
- 后到的运行默认**不等待**，立即以 `workspace_busy`（CLI 退出码 7）失败，
  错误信息里给出持有者的模式、计划名、PID/主机和开始时间，便于判断是谁占用。
- 加 `-wait-lock` 后，后到的运行在锁上排队，前一个结束后按序执行；等待过程
  响应上下文取消。
- `preview` 遇到占用立即返回 `workspace_busy`；`status` 始终只读可用。
- 锁随文件描述符由内核持有，持锁进程退出（正常或崩溃）即自动释放，不会出现
  永久死锁；持锁者信息记录在独立的 `.scaffold/lockholder.json` 中。

## 失败与中断后的恢复保证

- **运行前备份**：每次运行在 `.scaffold/backup/<run-id>/` 建一个运行级备份。
  任何计划路径在**第一次被写之前**，其当时的字节（或"当时不存在"这一事实）
  都被记录一次；并发 worker 因此不可能备份到写了一半的文件。
- 任意 `failed`、`skipped`（上游失败）或 `context` 中断：
  1. 停止派发新任务，等待已在执行的任务到达终态；
  2. 状态文件先记为 `rolling_back`；
  3. 依据备份还原：本次**新建**的文件删除（并清理遗留空目录），本次**覆盖**
     的文件用运行前的字节还原——磁盘回到运行开始前，不会留下半新半旧的
     产物；
  4. **不更新清单**（旧清单原封不动，清单写入本身是原子的）；
  5. 状态文件记为 `failed`/`interrupted` 并删除本次备份；
  6. 返回带分类的错误和报告（每个目标的 status、reason、摘要、attempt、
     最后步骤都可诊断）。
- **进程被硬杀（SIGKILL/断电）**：锁由内核自动释放；下次 `run` 获取锁后先做
  恢复检查——若状态文件停在 `running`/`rolling_back` 且留有备份目录，就先从
  最早的备份把工作区还原，再执行计划，并在报告的 `recovered` 中记录恢复了
  哪个残留运行。
- 若还原本身遇到 IO 错误，备份会被保留、状态停在 `rolling_back`，错误信息
  逐条列出需要人工处理的路径，而不是静默丢掉——不会出现磁盘、状态、计划
  三边对不上却无法判断的情况。
- 清单提交失败（写盘后）同样按备份还原，保证"产物已改但清单没更新"这种
  中间态不会外泄。
- 修正计划（或恢复上下文）后重试：因为磁盘与清单已回到一致的前态，重试
  会收敛到一个确定结果——要么成功（`created`/`verified`/…），要么明确指出
  需要人工处理的路径。
- 同一 `Engine` 实例维护内存中的 `attempt` 计数：目标**真正被处理**一次
  （包括处理后失败）计数 +1；仅被 `skipped` 不计。新进程/新 Engine 从 1
  开始。

运行错误分类（CLI 退出码）：

| kind | 含义 | 退出码 |
| --- | --- | --- |
| `invalid_plan` | 静态校验拒绝，未触碰文件。 | 2 |
| `render_error` | 模板解析/执行失败（含预演中发现的必然失败）。 | 3 |
| `write_error` / `manifest_error` | 文件 IO 或清单提交失败。 | 4 |
| `interrupted` | 上下文取消 / 收到中断信号。 | 5 |
| `partial_failure` | 存在失败/跳过的兜底分类。 | 6 |
| `workspace_busy` | 另一次运行正独占该工作区；本次未启动。 | 7 |

## 并发安全

- 跨工作区：POSIX `flock` 排他锁串行化不同进程/Engine/CI 任务对同一根目录的
  写操作；默认快速失败，`-wait-lock` 时排队。
- 同一 `Engine` 实例上的 `Run` 仍由实例级互斥量串行化。
- 单次运行内：调度共享状态由单一互斥量 + 条件变量保护；文件系统判定与备份
  由运行级锁串行化；状态文件的每次更新都是"临时文件 + fsync + rename"的
  原子写；attempt 计数有独立互斥量。`go test -race` 干净。

## 本地验证方法

全部离线完成，不需要网络：

```bash
# 单元测试（plan + engine，覆盖循环依赖/并发/预演一致性/部分失败恢复/
# 中断还原/删除与改动区分/重跑收敛/跨进程占用/硬杀恢复）
go test ./...

# 竞态检测 + 重复运行
go test -race -count=5 ./...

# 静态检查与构建（含 Windows 交叉编译）
go vet ./...
go build ./...
GOOS=windows GOARCH=amd64 go build ./...

# CLI 端到端
go run ./cmd/scaffold plan-example -o /tmp/plan.json
go run ./cmd/scaffold validate -f /tmp/plan.json
go run ./cmd/scaffold preview -f /tmp/plan.json -root /tmp/out   # 空目录：全部 created，不落盘
go run ./cmd/scaffold run -f /tmp/plan.json -root /tmp/out -workers 4
go run ./cmd/scaffold preview -f /tmp/plan.json -root /tmp/out   # 再预演：全部 verified
go run ./cmd/scaffold run -f /tmp/plan.json -root /tmp/out       # 再跑：全部 verified
go run ./cmd/scaffold status -f /tmp/plan.json -root /tmp/out    # phase/step/disk 一致性
# 第二个终端同时发起，立即得到占用结论（退出码 7）；加 -wait-lock 则排队：
go run ./cmd/scaffold run -f /tmp/plan.json -root /tmp/out -wait-lock
```

单测日志（`go test -v`）会打印每个场景的**输入计划**（`INPUT PLAN`）、
**处理顺序**与**最终状态**（`PROCESSING ORDER + FINAL STATE`，含每个目标的
status、SHA-256、reason、attempt）；预演用例额外打印
`PREVIEW PROCESSING ORDER + FINAL STATE`，状态用例打印每个目标的
lastRun/step/disk。
