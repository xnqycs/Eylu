# Bastion 宿主模式 1.0

Eylu 提供独立的标准流入口，复用 `agent.Conversation.Run`、工具调度器和共享 `RequestLifecycle`。模型请求、工具审批及真实执行、操作账本和检查点介质由宿主拥有。

```sh
go build -o eylu .
./eylu serve --transport stdio --protocol bastion-host/1.0
```

Windows 使用 `eylu.exe`。该命令不读用户或项目配置，不发现工作区、技能或 MCP，不注册本地工具，不读取 Provider 密钥，不监听端口。`--config`、`--workspace`、`--output` 不适用于此入口。stdout 只承载 JSON-RPC；诊断只写 stderr，不包含请求正文或参数。

## 协议与固定数据

- [protocol.schema.json](protocol.schema.json)：传输封装及 `$defs` 中的模型、工具、状态、事件、检查点和恢复 DTO。
- [fixtures.jsonl](fixtures.jsonl)：固定的请求、响应与通知示例；各条是独立示例，并非可直接灌入 stdin 的完整宿主程序。
- [acceptance.md](acceptance.md)：C01–C16 的测试位置、结果及平台限制。

生成并核对格式：

```sh
go run ./cmd/host-schema docs/host/protocol.schema.json docs/host/fixtures.jsonl
go test ./internal/host
```

协议版本为 `bastion-host/1.0`，状态版本为 `eylu-host-state/1`；其他版本明确拒绝。宿主请求 ID 使用 `h:`，引擎请求 ID 使用 `e:`。每个 UTF-8 JSON 对象独占一行，不支持 batch。最大消息为 16 MiB，包含结尾换行；初始化可协商至 1024 字节以上的更小值。所有字段精确匹配大小写，拒绝重复键、未知字段、缺失字段及不支持的枚举。可选字段明确标为 `omitempty`；空数组使用 `[]`，不能用 `null` 代替。

固定能力为 `host_model`、`host_tools`、`durable_checkpoints`、`interrupt`；`text_deltas` 必须协商。最多同时保有 1024 个对话和 64 个正在处理的入站 RPC；达到上限返回限制或忙碌错误。流输出积压有界，宿主必须一直读取 stdout。

## 宿主接入顺序

1. 启动子进程，同时读取 stdout、stderr。宿主等待请求响应时仍须处理反向请求。
2. `initialize` 协商版本、能力和消息限制。
3. `conversation.create` 提供 `conversation_id`、`instructions`、`context`。上下文条目为 `{"source":"ticket:123","text":"脱敏事实"}`，作为数据注入。先提交 `conversation.created` 检查点，再返回 `revision=1`。
4. `run.start` 提供完整输入、当前 `expected_revision`、新 `run_id`、模型/工具/目标绑定及所有正数限制。用户消息与启动事件提交后才返回受理结果。
5. 处理 `host.model.generate`、`host.tool.execute`、`host.checkpoint.commit`、`host.calls.cancel`。每个反向调用在派发前已持久化稳定身份。多个工具按模型返回顺序执行，每项结果提交后才执行下一项。
6. UI 通过宿主保存的事件读取事实。`engine.committed` 只是唤醒通知；`engine.progress` 和文本增量是可丢失的临时显示数据。
7. 用 `conversation.get` 检查运行、已提交版本、事件序号、未提交进度与阻塞项。完成后 `shutdown` 提供 `reason` 和 `wait_ms`，等待响应及进程退出。

`run.start` 的同 ID 同内容重送返回最初的受理版本，先于旧版本检查；同 ID 不同内容为 `ID_CONFLICT`。同对话只允许一个运行和状态写入者。绑定在一轮内不可替换，需先中断、核对，再用新输入启动新轮。

## 模型与工具

模型消息是独立的 `role/parts` DTO，角色为 `system/user/assistant/tool`，仅支持文本、工具请求和工具结果。工具调用 ID 在对话内必须唯一，历史压缩后仍保留已用 ID。模型适配器负责供应商原始 ID 和续接信息；不得向引擎返回私有推理过程。

`host.model.generate.purpose` 为 `conversation` 或 `compaction`，两者都消耗模型次数和输出预算。一次回调只允许一次供应商尝试；引擎不重试付费请求。宿主必须按 `remaining_limits` 和模型绑定执行真实预算。`usage.source=unknown` 或结果丢失会停止后续自动调用并留下待核对记录。`length` 和 `blocked` 输出只保留可用文本，丢弃工具调用及不完整参数，不能据此执行工具。

`input_schema` 采用 JSON Schema 2020-12 的明确子集，根必须是 object。支持 `type`、`properties`、`required`、`additionalProperties`、`items`、`enum`、`const`、数值上下界、字符串长度与 `pattern`、数组长度与唯一性、对象属性数量、`allOf/anyOf/oneOf`，以及 `title/description`。正则使用 Go 支持的语法。不支持引用、远程 Schema、format、默认值或其他未列出的关键词；不能静默删掉约束。引擎验证工具参数，宿主执行前仍须用原始 Schema 校验并完成自己的绑定、授权和审批检查。`read_only` 仅是提示。

工具结果保存 `status` 与 `effect`，不能据失败或取消推断回滚。`effect=applied` 的操作身份进入运行的 `known_effects` 索引，详情仍以宿主账本和 `call.settled` 为准。`status=unknown` 或 `effect=unknown` 会停止当前轮并阻止新轮，直到恢复证据解除阻塞。截断结果必须给出不透明 `resource_ref` 和 `truncation_reason`。

## 检查点与恢复

`host.checkpoint.commit` 必须在一个宿主事务内校验 `expected_revision/base_event_seq`、保存完整状态、按顺序追加事件，并返回新 `revision/event_seq/event_seq_range`。无事件时范围为 `null`。检查点内 State 的版本和事件序号是本次提交后的值。`checkpoint_id` 同 ID 同内容重送必须返回原结果；引擎只对检查点回执超时进行一次相同内容重送，不把此规则用于模型或工具。

状态保存当前消息上下文、压缩摘要、冻结绑定、预算、运行去重记录、已用工具 ID 和待核对调用。完整历史由宿主事件保存，压缩后检查点可丢弃已汇总的旧消息。状态超过消息上限时失败，不静默截断状态或丢弃待核对调用。`run.finished` 未成功提交时不会通知已提交终态，`conversation.get` 会显示未提交进度和恢复阻塞。

重启时先读取宿主最后一次成功提交的 State，再调用 `conversation.restore`，同时传入 `revision`、`event_seq`、`state_schema` 和 `reconciliations`。证据条目只有三种：

- `not_dispatched`：账本能够证明该请求从未派发；不能仅凭缺少结果作此判断。
- `settled`：按请求种类携带完整 `model_result` 或 `tool_result`。
- `unknown`：证据不足，继续阻塞。

恢复不调用模型或工具，也不恢复客户授权。未知旧运行处于 `stopping/reconciliation_required`；证据补齐后先提交旧轮中断结果，再接受新 `run_id`。如仍有未知结果，宿主读取最新恢复检查点、核对现场，随后再次提交恢复证据。相同版本但被改写的在线状态会被拒绝。

中断时宿主先冻结新派发并撤销对应租约，再发送 `run.interrupt`。引擎不再调度新调用，对在途请求发送取消，并在 `stop_grace_ms` 内等待原 RPC 的结算。取消响应仅表示收到请求；未决 ID 不能显示为“远端已停止”。已结束旧轮的中断重送不会影响新轮。

## 可运行的模拟宿主

```sh
go build -o eylu .
go run ./cmd/host-mock --engine ./eylu --ledger ./mock-host-ledger.json
go run ./cmd/host-mock --engine ./eylu --ledger ./mock-host-ledger.json --interrupt
```

Windows 把 `--engine` 改为 `./eylu.exe`。模拟宿主只使用内存中的假模型和假工具；单个 JSON 文件保存状态、事件、检查点去重与调用账本，临时文件同步后原子替换。事件序号是事件数组的一基索引。再次执行同一路径会恢复旧状态并开始新轮。一个账本文件只供一个宿主进程使用；此示例不是多进程生产数据库。

测试中的故障注入会在用户输入提交后、工具准备提交后、工具接收后分别终止测试自己启动的子进程。没有结果的已接收工具记录恢复为未知，不能自动重放。真实 Provider、客户设备执行器、UI、生产账本和对外 MCP 均属于 Bastion 的后续接入范围。

## 开发与验证

```sh
go test ./internal/host
go test ./internal/app -run 'TestC01C16NativeHostProcessIsolationRestartAndInterrupt|TestC11NativeKillAtDurabilityBoundaries' -v
go test ./...
go vet ./...
```

现有三平台 CI 的 `go test ./...` 会运行原生子进程测试，但本地通过不代表尚未运行的 CI 已通过。Go 内部包仍不是外部 SDK；宿主对接以协议 Schema 和版本协商为准。
