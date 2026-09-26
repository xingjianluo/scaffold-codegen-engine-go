# scaffold-engine

一个**纯本地**的声明式代码生成 / 脚手架引擎。同一份生成计划（plan）在重复执行、
并发执行和部分失败之后，都给出**可预期、可解释、可诊断**的结果。

- 多文件工程骨架生成，目标（target）之间支持依赖关系并按正确顺序调度。
- 循环依赖、缺失参数、未知依赖、目标 / 输出冲突在**写入任何文件之前**被明确拒绝，
  错误按类别（kind）区分，一次给出全部原因。
- 生成幂等且可复现：相同输入任意次执行，未被人为改动的产物内容与 SHA-256 摘要恒定。
- 对“使用者改动 / 外来文件 / 计划变更 / 已删除”给出**可区分**的处理结论；重复执行
  不会覆盖与本次计划无关的改动。
- 有界并发调度，共享状态受互斥与文件锁保护。
- 两阶段提交 + 崩溃日志（journal）：任一任务失败或被中断都不留下与计划不一致的状态，
  重试即可恢复到可判定结果。

所有生成与校验均在本地完成，**不访问网络、不需要凭据**。仅依赖 Go 标准库
（计划文件使用 JSON）。

## 目录结构

```
plan/    计划类型、JSON 加载与全部静态校验、错误分类（IssueKind）
graph/   依赖 DAG 与确定性拓扑排序
render/  text/template 渲染、模板 AST 缺参静态分析
state/   manifest 清单、摘要、状态分类、journal 崩溃恢复、工作区文件锁
engine/  并发调度、两阶段提交、回滚、中断恢复（核心）
main.go  本地 CLI
testdata/ 示例计划与模板
```

## 计划文件语义（JSON）

```json
{
  "name": "demo-service",
  "version": 1,
  "params": [
    { "name": "Service", "required": true },
    { "name": "Module",  "required": false, "default": "core" }
  ],
  "targets": [
    {
      "name": "readme",
      "template": "templates/readme.tpl",
      "output_path": "README.md",
      "params": { "Service": "billing" }
    },
    {
      "name": "config",
      "template": "templates/config.tpl",
      "output_path": "internal/config/config.go",
      "depends_on": ["readme"],
      "params": { "Service": "billing" }
    }
  ]
}
```

字段语义：

- `name` / `version`：计划标识，必填，`version` 必须为正整数。
- `params`：全局参数声明。
  - `required=true` 且没有默认值 / 有效值时，拒绝整个计划。
  - `default`：默认值；目标可在自己的 `params` 中覆盖。
- `targets[].name`：目标唯一名，重复即拒绝。
- `targets[].template`：相对计划文件目录的模板路径；必须是不含 `..` 的干净相对路径，
  且文件必须存在。
- `targets[].output_path`：相对工作区根（`--root`）的输出路径；规则同上，且不得位于
  引擎状态目录 `.scaffold/` 下。两个目标写同一路径即拒绝（输出冲突）。
- `targets[].depends_on`：依赖的目标名；未知名字、自依赖、环均拒绝。
- `targets[].params`：只能引用在顶层 `params` 中声明过的参数，否则拒绝。

模板使用 Go 的 [`text/template`](https://pkg.go.dev/text/template) 语法，并开启
`missingkey=error`：引用未提供的参数会报错而不是输出 `<no value>`。除渲染期保护外，
引擎在执行前还会解析模板 AST 做静态检查：

- `{{.Foo}}`（顶层 dot 上的字段）→ 参数 `Foo`。
- `{{$.Foo}}`（即使在 `range` / `with` 块内）→ 顶层参数 `Foo`。
- `{{range .Items}}{{.Name}}{{end}}` 中的 `.Name` 绑定到循环变量，**不**算作顶层参数；
  `else` 分支保留外层 dot。

静态检查会把“引用了未声明参数（`unknown_parameter`）”和“声明了但无值
（`missing_parameter`）”区分开。

### 拒绝原因（plan.IssueKind）

| kind | 含义 |
| --- | --- |
| `parse_error` | 文件不可读 / JSON 非法 / 未知字段 / `name`、`version` 不合法 |
| `duplicate_target_name` | 目标名（或参数名）重复 |
| `output_path_conflict` | 两个目标写到同一输出路径 |
| `unknown_dependency` | `depends_on` 引用了不存在的目标 |
| `cyclic_dependency` | 依赖图存在环（错误信息给出环路径，如 `x -> y -> x`） |
| `missing_parameter` | 必填参数无值，或模板引用了无值参数 |
| `unknown_parameter` | 目标参数 / 模板引用了未声明的参数 |
| `invalid_path` | 绝对路径、含 `..`、空段，或写入 `.scaffold/` |
| `missing_template` | 模板文件不存在或不可读 |
| `template_error` | 模板解析 / 渲染失败 |
| `empty_plan` | 计划没有任何目标 |

一次校验会**收集全部问题**（`ValidationError.Issues`），而不是只报第一个。

## 执行模型与确定性顺序

- 依赖图使用 Kahn 拓扑排序；就绪集合里永远选择**字典序最小**的名字，因此处理顺序与
  目标在文件中的声明顺序、map 迭代顺序、worker 数量无关，完全确定（例如
  `a` 先于其下游，`b/c` 之间按名字排序）。
- 渲染与暂存以有界并发执行（`--workers`），但一个目标只在其全部依赖成功后才开始；
  任一目标失败，其全部传递下游标记为 `skipped`。

## 产物状态判定与冲突规则

执行前会把每个目标“期望内容”的 SHA-256 与**上次清单**、**磁盘现状**比较：

| 状态 | 判定 | 默认处理 |
| --- | --- | --- |
| `missing` | 路径不存在，引擎也从未拥有它 | 写入 |
| `up_to_date` | 文件字节与期望内容一致 | 保留，不重写 |
| `plan_changed` | 文件与引擎上次写入一致，但当前计划渲染结果变了 | 安全重写 |
| `user_modified` | 引擎拥有的文件被外部改动（摘要不符）或被外部删除 | **拒绝**，`--force` 才重写 / 重建 |
| `foreign` | 路径存在但引擎从未拥有（即使内容恰好相同） | **拒绝**，`--force` 才采纳 / 覆盖 |

判定全部基于 SHA-256，给出可解释原因（含磁盘摘要与上次生成摘要）。重复运行只更新
计划内目标，**绝不触碰**计划外文件，也不删除从计划移除的目标的旧产物（那可能是用户
故意保留的）。

清单保存在 `<root>/.scaffold/manifest.json`，以目标名字典序规范化序列化，因此
manifest 的字节本身也可复现。

## 并发与失败收尾

- **工作区文件锁**：`<root>/.scaffold/run.lock`。同一工作区同时只能有一个引擎运行；
  并发的第二个实例立即得到 `workspace_locked` 错误，不会与持有方竞争输出。进程异常
  死亡留下陈旧锁时，删除该文件（或后续恢复）即可。
- 内存共享状态全部由互斥保护（测试使用 `-race` 验证）。

执行分两个阶段：

1. **阶段 A（并发，只动 `.scaffold/` 暂存区）**：渲染 → 分类 → 把变更写入临时文件、
   备份将被覆盖的现存文件。此阶段失败 / 被中断时，工作区真实输出与清单**完全不变**。
2. **阶段 B（串行，确定顺序）**：写 `journal.json` → 逐个 `rename` 安装 → 写新
   `manifest.json` → 删除 journal。所有安装均为同目录原子 rename（跨设备回退为
   原子写临时文件 + rename）。

中断 / 崩溃语义：

- 若在阶段 B 中途被杀，`.scaffold/journal.json` 仍然存在。下一次 `run`（或显式
  `recover`）会先回放日志：本次新建的文件删除、被覆盖的文件从备份恢复，使工作区回到
  **上一个已提交清单**对应的状态，然后再正常执行。
- 阶段 B 中若安装或清单写入失败，引擎依据内存中的同一日志立即回滚，并在回滚完成后
  删除日志。
- 重试总是达到一个可判定结果：要么全部 `up_to_date`，要么得到分类清晰、可解释的拒绝。

运行时错误类别（engine.ErrorKind）：`render_error`、`output_conflict`、`io_error`、
`interrupted`、`workspace_locked`。

## CLI 用法

```bash
go build -o scaffold-engine .

# 生成（校验 + 并发调度 + 提交）
./scaffold-engine run --plan plan.json --root ./workspace --workers 4

# 只计算每个目标的判定结果，不写任何文件（JSON 报告）
./scaffold-engine status --plan plan.json --root ./workspace

# 显式回放一次中断日志（通常 run 会自动做）
./scaffold-engine recover --root ./workspace

# 不修改任何输出，打印将要发生什么
./scaffold-engine run --plan plan.json --dry-run

# 采纳 / 覆盖被用户改动或外来的文件
./scaffold-engine run --plan plan.json --force
```

`run` 将 SIGINT / SIGTERM 转换为优雅取消。

## 本地验证

无需任何外部服务或凭据：

```bash
# 全量单测（含循环依赖、并发调度、部分失败恢复、中断恢复、竞态检测）
go test ./... -race -count=1

# 重复执行以排查偶发问题
go test ./... -race -count=5

go vet ./...
```

测试覆盖的关键场景：

- `plan`：循环依赖（含环路径报告）、自依赖、缺必填参数、未知依赖、重名、输出冲突、
  路径逃逸、空计划、未知参数、模板缺失。
- `graph`：确定性拓扑顺序（多次运行一致）、传递依赖者集合、环拒绝。
- `render`：确定性渲染、AST 缺参检测（含 `if/range/with` 与 `$` 语义）、
  `missingkey=error`。
- `engine`：依赖顺序、幂等与摘要稳定、真实并发（b/c 同时渲染）、用户改动拒绝且
  `--force` 恢复、外来文件拒绝、阶段 A 失败不留半成品且下游 `skipped`、阶段 B 崩溃
  日志回放、上下文取消、同工作区并发实例的锁拒绝。
- `state`：manifest 确定性序列化、journal 新建删除 / 备份恢复、锁互斥、摘要稳定。

单测日志会打印输入计划、处理顺序（`PROCESSING ORDER`）与最终每个目标的状态
（`FINAL STATE` / `final-status`）。

端到端手工验证见 `testdata/smoke`。
