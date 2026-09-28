# 宿主接入 v1 验收记录

验证日期：2026-09-29。基线为 `8eb4a2c`，结果来自本次宿主接入实现。协议为 `bastion-host/1.0`，状态为 `eylu-host-state/1`。本机为 Windows/amd64，Go 1.25.13。

E1–E6 的源码、协议 Schema、固定夹具、模拟宿主和兼容性说明均已交付。C01–C15 模拟链路及 Windows/amd64 的 C16 原生子进程验证通过；macOS/Linux 原生验证仍未完成，不能据此声明这些平台已达到产品交付条件。

## 合约矩阵

以下名称位于 [contract_test.go](../../internal/host/contract_test.go)、[reliability_test.go](../../internal/host/reliability_test.go) 和 [host_process_test.go](../../internal/app/host_process_test.go)。所有调用使用合成数据、假模型和假工具，无真实客户设备或付费模型调用。

| 编号 | 结果 | 测试与证据 |
| --- | --- | --- |
| C01 | 通过 | `TestC01C16NativeHostProcessIsolationRestartAndInterrupt`：在子进程的用户/项目目录放置无效配置，注入假 Provider 密钥及不可达端点，仍完成宿主回调，stderr 为空。 |
| C02 | 通过 | `TestC02EmptyCatalogAndUnknownTool` 验证空目录和未知工具；原生测试清空 PATH 并设置不存在的 SHELL/COMSPEC，仍可完成对话。 |
| C03 | 通过 | `TestC02C03C04HostLoopAndDurableOrdering`：模型→两个顺序工具→最终模型；逐次检查准备与结果已持久化，绑定、参数、身份和工具结果不变。 |
| C04 | 通过 | 同上：首条指令来自宿主，无本地编程角色；原生无本机工作区/工具装配。上下文作为数据注入。 |
| C05 | 通过 | `TestC05DeltasAreEphemeralAndDeduplicated` 验证重复与迟到片段不转发，最终消息替代临时片段；`TestC05LengthRetainsTextAndDiscardsPartialToolArguments` 验证截断文本可保留、不完整工具参数不执行。 |
| C06 | 通过 | `TestC06DeniedResultIsPreserved`、`TestC06ApprovalTimeoutCannotBeRevivedByLateResult`：审批拒绝保留无副作用语义；超时后实际送达迟到结果，运行及版本不复活。原生测试覆盖审批等待中的主动中断。 |
| C07 | 通过 | `TestC07InterruptWhileInputCommitOrModelIsWaiting`、`TestC07InterruptCollectsSettledResultAndDoesNotStartNextTool` 及原生审批中断：派发前、模型中、审批中、工具中停止；下一工具不启动，结算保留部分副作用。 |
| C08 | 通过 | 上述工具取消测试保留 `effect=applied`；`TestC08UnknownEffectsBlockNewRunAndNeverRetry` 验证未知效果进入未决列表、阻止新轮、不重试。 |
| C09 | 通过 | `TestC09ConcurrentStartsHaveOneWriter`、`TestC09IdempotencyPrecedesRevisionAndConflicts`：竞争只接受一轮；同 ID 同内容先于版本检查，同 ID 异内容及新 ID 旧版本均明确拒绝。 |
| C10 | 通过 | `TestC10CommitAcknowledgementLossReusesIDAndEvents` 验证提交回执丢失后同 ID 重送不重复版本/事件；准备及终态提交失败均阻止继续或冒报完成；恢复测试拒绝不兼容状态版本。 |
| C11 | 通过 | `TestC11NativeKillAtDurabilityBoundaries` 在用户提交后、工具准备后、工具接收后杀掉测试子进程并重新启动；输入保留，未知工具不重放。`TestC11RestoreToolResultFromLedgerAndNeverReexecute` 验证已结算账本结果及副作用恢复。 |
| C12 | 通过 | 并发开始、回执丢失和重启测试验证 CAS、连续序号；`TestC12CommittedHistorySurvivesLostNotifications` 丢弃全部临时通知，仍可从已提交事件重建用户及最终消息。 |
| C13 | 通过 | `TestC13C15BudgetsTruncationAndUnknownUsage`、`TestC13ElapsedLimitCancelsOriginalCall`、`TestC13CompactionUsesHostAndCountsBudget`：模型次数、输出、总时间限制生效，压缩通过宿主且计预算并持久化。 |
| C14 | 通过 | `TestC14StrictFieldsAndProtocolErrors`、`TestC14NegotiationAndUnsupportedContentFailClosed`、`TestC14BoundedUnterminatedMessage`、`TestC14ServeStartupErrorsStayOnStderr`：版本/能力/严格字段校验，16 MiB 无换行输入有界失败，错误不泄露正文，启动错误只走 stderr。 |
| C15 | 通过 | `TestC13C15BudgetsTruncationAndUnknownUsage` 的 unknown/revoked 分支及模型中断/恢复测试：调用失败或成本未知保留未决记录，每次只尝试一次，不切换模型。绑定撤销由假宿主以模型请求失败返回。 |
| C16 | 部分平台通过 | Windows/amd64 已完成原生启动、双向 RPC、重启、审批中断、正常退出及强制终止恢复；其他平台见下表。 |

`schema_test.go` 另行验证生成文件与 DTO 一致、固定消息符合 Schema、缺失字段被拒绝，以及不完整工具参数只允许出现在停止输出中。

## 平台证据

| 平台 | 根程序编译 | 原生子进程与标准流 |
| --- | --- | --- |
| Windows amd64 | 通过 | 通过；原生测试与实际 `eylu.exe` 模拟宿主两种路径均运行 |
| Windows arm64 | 交叉编译通过 | 未验 |
| Linux amd64 | 交叉编译通过 | 未验 |
| Linux arm64 | 交叉编译通过 | 未验 |
| macOS amd64 | 交叉编译通过 | 未验 |
| macOS arm64 | 交叉编译通过 | 未验 |

实际 Windows 程序的同一模拟账本连续执行结果：

| 执行 | 最终状态 | revision | event_seq | 模型次数 | 输入/输出 token |
| --- | --- | --- | --- | --- | --- |
| 新对话、工具、最终回答 | completed | 9 | 13 | 2 | 40 / 16 |
| 重新启动、恢复、审批中断 | interrupted | 15 | 22 | 1 | 20 / 8 |

第二轮原因是 `mock_operator_takeover`，未决请求为空。测试只证明本例模拟执行器能够结算取消，不代表真实远端收到取消后必然停止。

现有三平台 CI 会通过 `go test ./...` 执行原生子进程测试；本记录没有声称尚未运行的 CI 已通过。交叉编译不能代替原生验收。

## 历史可靠性事项

| 原评估中的事项 | 宿主入口处理与证据 |
| --- | --- |
| 用户输入可能未先持久化 | 新增独立的用户提交钩子，输入与 `run.started` 提交后才确认受理、开始模型请求；派发前中断与原生杀进程测试验证恢复。 |
| 多写者导致事件序号竞争 | 每个对话只接受一个运行；检查点写入串行化，宿主 CAS 同时验证版本与基础事件序号；并发/回执丢失测试覆盖。 |
| 配置影响权限或 Provider 路由 | `serve` 从入口绕开本机配置、Provider、shell 和工具发现；宿主提供的绑定在一轮内冻结。原生隔离与空工具测试覆盖。 |
| 部分失败没有完整运行报告 | 宿主终态单独持久化 usage、known_effects 和 unresolved_request_ids；终态提交失败则暴露 stopping/checkpoint_failed，不声称已提交完成。 |

这些证据针对新宿主入口。现有 CLI/TUI 的全仓回归通过，并不代表原评估中的所有 CLI 问题已在本次全面修复。

## 可复现命令与验证范围

```sh
go run ./cmd/host-schema docs/host/protocol.schema.json docs/host/fixtures.jsonl
go test -p 2 -count=1 -timeout=180s ./...
go vet ./...
go mod verify
go build -trimpath -o eylu.exe .
go run ./cmd/host-mock --engine ./eylu.exe --ledger ./host-ledger.json
go run ./cmd/host-mock --engine ./eylu.exe --ledger ./host-ledger.json --interrupt
```

本次全仓测试、`go vet ./...`、`go mod verify` 均通过。最终 Schema 修订及迟到响应/通知丢失断言后，受影响的 `internal/host` 测试和 vet 再次通过。六个编译目标均使用本次源码；Windows 原生程序使用 `-trimpath` 构建。

尝试运行 Go race 检查，但本机没有可用 C 编译器且 CGO 未开启，Go 在测试启动前报告 `-race requires cgo`；因此 race 未验，没有安装编译器或将其记录为通过。

模拟宿主 JSON 账本仅支持单宿主进程，不能代替生产数据库事务或客户操作账本。首版未支持的媒体、Schema 关键词及其他能力会明确拒绝，详见 [接入说明](README.md)。真实 Provider、客户设备、生产持久化、审批界面、三平台产品发布验收属于 Bastion 后续接入工作。
