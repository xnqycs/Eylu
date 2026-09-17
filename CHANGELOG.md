# Changelog

## Unreleased

- **中断后停止批次调度（修复）**：批次在运行期接受用户中断（宿主工具通过 `ControlReporter` 返回 `ControlInterruptRequest`，例如关闭 `ask` 提问）后，此前只有预检阶段会阻止后续调用，调度循环仍会启动同批次尚未运行的调用——实测 `[ask, write_file]` 在关闭提问后文件仍被写入、`[ask, bash]` 命令仍被执行。"是否允许启动下一个调用"收敛为单一判断 `batchStopped`（同时考虑用户中断、请求取消与审批/检查点/调度等基础设施故障），未启动调用统一闭合为 `not_executed` 并在结果 metadata 上带 `interrupt_request`；请求级终态优先级固定为 abort > cancel > interrupt > continue，中断在败给更高优先级时仍保留在调用状态与结果上。

- **意图收集严格无副作用（修复）**：`ReportIntent` 此前经由 `forWrite` 解析目标路径，而该路径在 `create_parent_dirs` 为真时会 `MkdirAll`——意图写入失败会留下新建目录，"未执行"只说对了一半；它还用 `context.Background()` 掩盖请求取消。现在新增无副作用的 `pathResolver.writeTarget`/`resolvedDirectory`（保留原有符号链接与越界校验），`MkdirAll` 只发生在执行阶段且位于意图持久化之后；`IntentReporter` 契约改为接收请求 context，读取原文件证据遵守既有 `previousHashMaxBytes` 上界。批量意图写入也不再描述一个它无法知道的状态：同一批次内命中同一目标路径的调用退回逐调用写入（在自身执行前读证据），不同目标仍保留单次批量 append。

- **日志物理水位与逻辑去重分离（修复）**：去重此前把被跳过记录的物理位置一并丢弃，于是中间重复事件让加载报 `session event sequence gap`、会话直接不可读，末尾重复事件让下一次 `Append` 复用日志已有的序号（再次加载即缺口）。现在去重只阻止内容被应用两次，被跳过的有效记录仍推进快照水位；`Load` 以最后一条有效物理记录作为追加位置，`Append` 在"整批都是重试"时也填充该缓存，`Save` 不再用可能落后于日志的快照水位驱动追加。磁盘 schema 未改动。

- **Anthropic 系统段完整保留（修复）**：Anthropic 只有一个 `system` 字段，而会话会产生多个 system turn（基础提示、MCP 指令与资源、Skill 目录与正文、任务列表、项目图、压缩摘要）；此前每个 system turn 直接覆盖该字段，实际只发送最后一段——在压缩过的会话里就是摘要，模型因此完全丢失系统提示。现在按顺序累积并以内容块数组一次写入（保留段边界），空 system 段不产生空块。该要求已进入公共 Driver 契约（`TestDriversKeepEverySystemSegmentInOrder`，覆盖 `openai_chat`、`openai_responses`、`anthropic_messages`、`mistral_conversations`、`perplexity_agent`），并确认其他适配器没有同类覆盖、遗漏或角色降级。

- **TUI 与 CLI 同享检查点与运行摘要（修复）**：TUI 此前未设置 `executor.Checkpoint`，副作用在日志里只留下 `tool_prepared` 与 turn append，没有意图与完成记录；运行摘要也只存在内存中、重启即失。现在两个入口共用同一套接线（见"共用请求执行服务"一条），TUI 也持久化 `run_reported`，且先写摘要再 Sync。同一处调查发现父子代理共用调用 ID 空间：general 子代理与父请求在同一会话内可能拿到相同 `call_id`，此前会把子代理意图判为"同 ID 不同内容"的冲突并直接拒绝执行。生命周期事件身份改为按请求限定（`intent:<request_id>:<call_id>`、`prepared:`/`completion:` 同理），逐调用意图路径补上此前完全缺失的稳定事件 ID，`session.ToolCompletion` 新增增量字段 `request_id`，`SamePendingCall` 在任一侧无 request_id 时退化为仅按 call_id 匹配，旧日志行为不变。

- **统一请求收尾与用量入账（修复）**：非法模型响应（工具参数不是合法 JSON、同一请求内重复调用 ID）此前直接 `return last, err` 绕过 `runFinalizer`，于是运行摘要只有 request ID、停止原因为空，累计用量为零，宿主事件队列不 flush，pending 集合留给下一个请求。该出口现在走统一 `finish`。同时：主模型调用的用量提前到调用返回后立即记录（不再以 turn 成功持久化为前提），收尾阶段再次失败的事件投递不再静默丢弃——原始错误仍是原因，第二个失败经 `errors.Join` 与运行摘要 `warnings` 一并报出。

- **会话轮换取得所有权、压缩移出状态锁（修复）**：`NewSessionWithEnvironment` 此前只 `await`（等待）而不占位，在"旧请求结束"与"轮换写入"之间存在窗口，新请求可在此启动、把消息写进即将被整体替换的会话；现在 `runGate.acquire` 在同一把锁下完成"等待+占位"，轮换期间 `begin` 返回 `ErrConversationBusy`，手动 `Compact` 同样取得所有权（运行中压缩请求会得到 busy 错误）。压缩摘要此前在状态锁内执行——手动压缩还在锁内做上下文窗口解析与宿主回调，回调读会话会直接死锁——期间 `ContextReport`/`ExportState`/`RequestStop` 全部阻塞；现在拆为"锁内确定性规划 → 锁外摘要模型调用 → 锁内校验并提交或拒绝 → 锁外交付回调"，快照已被取代时放弃该次压缩。

- **统一模型可见的工具结果投影（修复）**：驱动此前把 `content`/`content_blocks`/`structured_content` 三个字段一起序列化进请求，而上下文账本只按 `content` 计费、裁剪也只裁 `content`——一个 1 MiB 图片块以 base64 进入文本字段，却只按几百字符计费（实测账本 7 字节 vs 请求 1,048,576 字节），MCP 结果还会把同一段文本发两遍。现在 `protocol.ProjectToolResult` 是唯一定义：重复文本块丢弃、二进制块只保留类型/MIME/字节数/摘要与说明、超限结构化内容整体替换为合法 JSON 摘要、超限文本按 rune 边界裁剪、块数与结构化内容有上界；账本、裁剪与驱动使用同一份投影，历史仍保留完整结果。

- **预算准入与子代理用量（修复）**：压缩摘要是付费调用，此前在任何预算准入之前无条件发起，被预算拦下的请求已经为摘要付过费；现在上下文层接收 `admitSummary`（即 `budget.admits`），不足时改用确定性压缩，手动压缩不传准入（没有请求预算）。子代理此前只上报最后一次模型调用的用量（实测两次调用报 50/5 而非 150/15，因为第一次调用的用量被丢弃），现在读取整轮累计用量，并新增增量字段 `AgentTaskResult.model_calls`/`AgentTask.model_calls` 单独报告子代理成本。已复核无需改动：provider 未报告 usage 时调用次数照常计数、总量标记为估算。

- **CLI 与 TUI 共用请求执行服务（重构）**：两个入口各自复制了同一段持久化逻辑（执行器身份与检查点、turn/prepared 回调、请求开始、请求超时、运行摘要、Sync 与错误优先级），TUI 缺少检查点与运行摘要正是这段复制造成的。现在集中到 `internal/app/request_runner.go` 的 `toolRun`（`executor`/`requestContext`/`prepare`/`settle`）与 `requestMetric`，前端只保留交互与展示，旧的重复路径删除。新增跨入口一致性测试：同一份脚本化模型分别驱动 `eylu chat` 与 TUI，比较调用终态、运行摘要计数、turn 角色序列、停止原因、用量与生命周期事件，同时忽略文本排版、时间戳与 ID；每个场景另带绝对契约（临时移除检查点绑定会让它失败），因此它也能发现"两个入口同样出错"，而不只是发现分叉。

- **命令分类按实际执行 shell 的方言解读（安全修复）**：分类器此前一律按 POSIX 引号规则解读命令行，而 Windows 回退 shell 是命令解释器；后者没有单引号成组，单引号内的 `&`、`|`、`>` 仍是分隔符或重定向。实测（非破坏性探针 + 临时目录）此前会把这类行判为只读并自动放行，而 `cmd` 实际执行了第二条命令、甚至创建了文件。现在 `policy.Config.Shell`（`ShellPOSIX`/`ShellCommandPrompt`）是显式输入，`tool.ActiveShellDialect()` 报告本进程将使用的 shell；命令解释器分支刻意保守（单引号不成组、`%` 展开、`^` 与括号一律拒绝），无法证明即回退 `unknown` 并要求确认。对照测试带正向对照（未加引号的分隔符必须被 shell 观察到 active），避免"什么都没观察到"被当成"一致"。

- 文档：新增 `docs/architecture-and-reliability.md`（架构图、请求生命周期与所有权、工具执行阶段与检查点顺序、日志水位与逻辑去重与恢复规则、两个入口的共同行为与允许差异、上下文投影与预算定义、应用层权限与 OS 沙箱边界、不变式到测试的映射、已知限制）；README 补充"命令按实际执行 shell 的方言解读"的规则，并新增"请求生命周期与入口一致性"小节。

- 去重收益修正：只有引用**确实比正文小**时才替换（按字节比较，避免估算器把极小正文四舍五入成 0），否则保留正文；跳过次数记为 `NotWorthReplacing` 并写在 block 的 `deduplication_not_worth_it` 上。PR-05 若干用小正文断言"会去重"的用例改为使用"明显大于引用的正文"（考规则而非 fixture 大小），断言"保留正文"的用例同样放大以保持判别力。

- §8.3 第 1 条已实施：超长单行被在行内切开时改为报告**行内字节区间**（`retained_bytes`），覆盖判定扩展为"行区间 或 行内字节区间"，重复读取的 token 从 512 降到 198（4000B 单行 / 512B 预算，省 314）。红线保护：行内字节区间永不被当作行区间（部分行的 canonical 不覆盖整行读取）、只有被完全包含时才用引用、行内片段永不取代其他 canonical、引用文本写明"部分行"；负向用例与正向用例同为验收条件。

- 重复读取去重收益实测（真实读取、逐形状）：超长单行 40,001→~173 token（+39,828），而小正文去重是**净亏**（引用长度基本固定 ~173 token，比正文大）；据此记录"去重总是省 token"不成立。同时更正：**A11 的前提成立**——最初测量跳过了 contextualizeTurn 的裁剪步骤，补上后复现（4000B 单行在 512B 预算下于行内被切开、无 etained_ranges、重复读取每次仍付 512 token），§8.3 第 1/2 条是真需求但尚未实施（要改覆盖判定本身，属红线 1 保护区域）。

- 大日志加载与恢复有量化基准与阈值：首次 Append（建索引）10^4 事件 90.6ms、10^5 事件 895ms，Load 55.0ms / 511ms，曲线线性（×10 事件约 ×10 代价），因此"增量/窗口索引"当前不需要；断言含 5 秒时间预算与每事件 60 次的分配上限（实测 22.0 次/事件，分配次数是确定性的），并断言大日志只改变代价、不改变结论。

- soak 与压力测试：新增 `internal/tool/soak_test.go`（冲突资源并行批次、反复取消、每轮注入审计/checkpoint 故障；默认 6 轮，`EYLU_SOAK_ROUNDS` 可延长），断言终态闭合、协调器不残留 waiter、无 goroutine 增长、每调用恰好一次意图与 completion；泄漏检测器自身有反证测试；"同路径不得重叠"这条搜索的已知局限写在 README 与测试注释里。

- POSIX 运行时验证：新增 `//go:build !windows` / `unix` 的显式测试（`internal/session/replace_other_test.go` 的原子替换与目录 fsync、`internal/tool/process_tree_unix_test.go` 的进程组取消），它们在 CI 的 Linux/macOS leg 上真实运行；README 写明"本机能做的最强验证只是交叉链接，能编译不等于已验证"。

- 累计用量与指标口径：`LoopOptions.Usage` 补上生产调用点（此前累计数字根本没被采集），两套口径命名分开——`usage` 是最后一次调用、`request_usage` 是整次请求；`json` 在不改名任何既有字段的前提下新增顶层 `request_usage`/`request_model_calls`，`jsonl` 新增独立 `request_usage` 行，metrics 与 `Summary` 分别记录并累计两者，`/run` 显示累计口径。
- 四种输出一致：判词集中到 `agent.RunStopNote`（覆盖 `length`/`cancelled`/`error`/`token_budget`/`iteration_limit`/`policy_tightened`/`persistence_failed`/`event_sink_failed`），TUI 历史对非正常结束写出一行说明、计时行改为 `Stopped after`（不再用 "Completed in" 为截断答案计时），CLI 与 TUI 不再各说一套；新增 `/run` 展示最近一次运行摘要（`stop_reason`、调用次数与各终态计数、token 与缓存命中、恢复诊断、`warnings`），内容取自运行摘要本身。
- 补齐生命周期事件表：新增 `request_started`（首次模型调用前写入）与 `tool_prepared`（以 pending 集合为来源，携带调用/请求/轮次），并新增可选 `request_id` 事件字段；两者是**证据而非状态**——不改写快照、不改变恢复结论，存在意义是让"已准备但从未写入意图"的调用可判定为未执行。§13.3 的 `model_turn_committed` 由 `turn_appended`（逐 turn 落盘）承担、`request_finished` 由 `run_reported` 承担，README 给出完整对照表。
- pending 视图：`Conversation` 为当前请求维护显式的已提交调用集合（`PendingCalls()` / `OpenPendingCalls()`：调用 ID、工具、父调用、turn 与轮次、是否已被执行器接受、终态），终态闭合改为消费该集合而不再扫描 transcript（扫描只保留在恢复路径）；请求结束时集合必为空，运行摘要新增 `pending_at_end` 与对应 `warnings`，把"仍有未闭合调用"作为缺陷信号报出来。
- 检查点成本与失败语义：一个批次里 N 个副作用调用的意图合并为一次 append（仍在任何调用开始之前写入），N=3 从 2N=6 次降到 4 次；completion 写入失败时结果保留在内存并进入待补偿队列，下一次成功的 append 补写同一调用 ID 的 completion（事件 ID 由调用 ID 派生，幂等），因此瞬时写失败不再留下永久 `outcome_unknown`；恢复结论改为三态并必须指出依据（内容哈希一致 → 未发生；不一致 → 未知并给出两个哈希；读不到或无哈希 → 未知且说明是无证据），任何情况都不重放；`write_file` 目标超过 4 MiB 时只记录 `weak:size=…:mtime=…` 弱证据并在诊断中明确标注为提示而非证明。
- 旧日志重复 turn 改为保守诊断：没有事件 ID 的日志在两条序列上写出同一 turn ID 时，内容一致的只应用一份并产出 `benign` 诊断，内容冲突的保留第一份并产出需要人工确认的诊断；两种情况都不改写原始日志。诊断分级后，`benign` 的诊断不再阻断 `--resume`，重复 turn 的错误信息带上会话 ID、turn ID 与恢复建议。
- 补齐预算与用量口径：新增 `cached_input_tokens`（`protocol.Usage`、`RunUsage` 与运行摘要），各适配器映射 provider 的缓存明细（Chat 的 `prompt_tokens_details`、Responses 的 `input_tokens_details` / `prompt_tokens_details`、Anthropic 的 `cache_read_input_tokens`）；它明确为 `input_tokens` 的子集，不改动预算总额；准入拦截（`max_total_tokens` 小于提示词估算）现在统一记为 `stop_reason=token_budget`，README 给出判断方式与示例。
- 隔离宿主回调故障：审计 sink 的失败或 panic 不再穿透请求（不改变调用终态、不重试、不阻塞、不杀死请求），计入运行摘要 `audit_failures` 并在首次失败时于 stderr 出一条诊断；事件投递分为关键事件（同步按序、失败即终止请求）与流式增量（可合并、慢消费者超预算时丢弃并计数 `events_dropped`），运行摘要新增 `warnings` 说明这类不影响终态的故障。
- 会话改为逐 turn 落盘：模型 turn 与工具 turn 一提交就同步写入事件日志，不再等请求结束的同步，运行中途崩溃最多丢掉仍在进行中的那一轮；turn 事件身份即 turn ID，增量写入与随后同步重放是同一个事件 ID，日志识别为重试而不重复；提交钩子失败按持久化故障处理（保留内存结果、不再启动新的副作用、`stop_reason=persistence_failed`）。
- 安全收紧改为在当前请求内生效：把模式由宽变窄、新增 `deny_tools`、禁用 MCP server 或把 Web 权限收为 `deny` 时，正在运行的请求会在下一个批次边界之前停止，未启动的调用闭合为 `not_executed`，已产生的副作用与结果保留；运行摘要记为 `stop_reason=policy_tightened` 并附原因，TUI/CLI 显示"已按新设置停止当前请求"。放宽只对下一个请求生效。
- 统一 Provider 停止原因映射：所有适配器共用一处策略表（`internal/driver` 的 `StopKindFor`），各自只负责把方言翻译成统一词表；`completed` 且带工具调用、`tool_use` 却没有调用、以及无法识别的取值都按协议错误拒绝，不再默认当作完成。为只返回 `finish_reason: "stop"` 的网关新增按 Provider 配置的放宽开关 `accept_tool_calls_with_stop`（默认关闭），开启后按 `tool_use` 执行并留痕在响应、运行摘要 `interop` 与审计中。

- 增加跨 Provider 的 hosted `web_search` / `web_fetch` 协议、目标能力解析、稳定工具规划、流式生命周期、引用、Web usage 和原始 Provider metadata；新增 Responses、Chat、Messages、Interactions、Conversations 与 Agent wire mapping。
- 增加 delegated 与 MCP client fallback、提交级 `max_uses`、可选 Web 审批、URL/域名/公网地址校验、可信网络边界、TUI 活动与引用展示，以及 JSON/JSONL、指标和审计投影；Web 默认直接执行，兼容 Responses 中转上的 GPT 搜索支持单批最多 10 条客户端并发扇出与稳定归并；TUI 展示批量查询词和打开 URL，并以最多 5 项的动画窗口折叠旧活动，隐藏计数行支持点击展开。
- 修复恢复会话后的空历史视图：TUI 和 `--no-tui` 交互模式回显用户、助手与工具历史，TUI 默认定位到最新内容。
- 修复 MCP 管理体验：启动加载期间在 Banner 下展示动画并在终态后清除；完整输入 `/mcp` 后可直接打开，详情页支持左右方向键切换，Tools 页仅展示工具列表，按 Enter 进入工具详情、Esc 返回；连接错误进入内容区并去重显示，原始配置与诊断 JSON 不再挤占详情页或输入区。
- 优化 MCP 启动与连接稳定性：TUI 首轮请求复用启动时建立的 manager，多个 server 最多并行连接 4 个；Streamable HTTP 握手中的每个 POST 独立应用 60 秒期限并携带稳定 User-Agent，工具目录就绪即进入 connected，日志级别、资源、资源模板和提示词改为后台加载；临时连接错误最多自动重试 3 次，最终失败后可手动执行 `reconnect`，退出清理采用有界等待。

兼容性：生命周期事件 ID 形状变化（`intent:`/`prepared:`/`completion:` 现在携带 request ID，逐调用意图路径此前完全没有事件 ID），增量字段 `ToolCompletion.request_id`、`AgentTaskResult.model_calls`、`AgentTask.model_calls`，以及导出配置字段 `policy.Config.Shell`（零值为 POSIX）；磁盘 session schema、protocol v1 与既有字段语义未变。

行为变化：Windows 且无 git-bash 时，单引号内的分隔符以及 `%`、`^`、括号不再自动放行，改为要求确认；运行中 `/compact` 返回 `conversation already has a running request`；模型可见的工具富结果变为有界投影（图片以描述+摘要代替 base64）；子代理用量口径由"最后一次调用"变为"整轮累计"，报表数字会变大；非法响应现在会写入运行摘要（`stop_reason=aborted`）并释放 pending，此前这些字段为空。

已知未修复（本轮发现，尚未处理）：请求 context 在开始前就已被取消时，会以 `config_error: context canceled` 失败并被记为 `aborted`，且不写入 `request_started`/运行摘要/turn（CLI 与 TUI 行为一致，两个入口都已在此契约下断言）；plan（isolated profile）模式请求的会话日志不含 user turn，只含模型/工具 turn；plan 模式下写操作因工具不在 registry 而被报为 `failed` 而非 `rejected`，且该调用在事件日志中没有任何记录（只有运行摘要计数）；取消发生在模型调用进行中时报告 `aborted` 而非 `cancelled`；项目地图扫描仍在状态锁内；`EYLU_SHELL` 以 `-lc` 调用，因此仅适用于 POSIX 兼容 shell，指向其他 shell 时仍按 POSIX 规则解读。

验证边界：本机无法运行 `go test -race`（无 C 编译器），因此竞态由 CI 的 `Race and static analysis` 作业发现：首次推送时该作业报告了测试替身 `orderedSink`/`faultSink` 的写竞争，以及跨入口取消场景对时序的依赖，两者均已修正并重新推送。除此之外，本轮在本机 Windows（go1.25.8）上执行了 `scripts/verify.ps1` 的全部阶段（gofmt 两种口径、`go mod verify`、`go vet ./...`、staticcheck v0.7.0、third-party notices、actionlint、`go test ./...`、`go build`、`smoke.ps1`、`smoke.sh`）并通过；Linux/macOS 原生运行、`go test -race`（本机无 C 编译器）与定向 fuzz 未在本地运行，由 CI 覆盖或记录为未执行；driver 验证全部离线，无真实 Provider 联调，也没有落盘的 SSE fixture 文件。

## v1.0.0-rc.2 - 2026-07-21

- 增强上下文压缩与手动触发能力：支持 `/compact`，改进压缩预算、摘要恢复、工具调用原子组保留和 TUI 压缩反馈。
- 增强工具调用并行调度：在 Driver 与工具均声明并行能力时并发执行连续只读工具，保持稳定结果顺序并完善取消、超时与 panic 隔离。
- 将 `--resume` 调整为按 session ID 精确恢复，补齐缺失、损坏、跨工作区、空 ID 等严格错误处理和交互退出恢复提示。
- 完成 MCP 客户端能力：覆盖 stdio、Streamable HTTP、SSE、OAuth、会话恢复、动态目录、tools/resources/prompts、roots/sampling/elicitation 与 CLI/TUI 管理。
- 完善中英文使用文档、发布说明和跨平台 smoke 校验。

## v1.0.0-rc.1 - 2026-07-20

- 重构 TUI 启动与运行反馈：加入加宽粗体斜体 Eylu 字符画、版本和工作目录 Banner；增加默认关闭的 `/gradient` On/Off 选择器及 `gradient_enabled` 持久化配置，启用后 Banner 与底部状态栏以约 20 FPS 显示主题强调色的逐字符 ANSI 真彩单色流光；新启动或 `/new` 后在首个 Prompt 前将 Context 展示为 100% 可用，之后按真实剩余/已用比例和友好状态短句展示；activity 行将 reasoning token 改为 `thinking` 与整秒 `thought for` 用时；`/context` 增加 Signal Strip、分类聚合和可滚动详情；连续工具组与后续消息之间增加留白。
- 增加 Provider 级 `reasoning_effort` 与 `/effort`：按模型档案提供 `auto` 至 `ultra` 动态选择器、当前项标识、右上角状态、TOML/session 往返和模型切换原子回退；Responses 使用 `reasoning.effort`，Chat Completions 使用顶层 `reasoning_effort`，`auto` 省略请求字段。
- 修复 TUI 文件引用、输入导航与完成指标：裸 `@path` 支持 ignored 文件的精确或唯一名称解析，`read_file` 卡片显示真实字节/行数；session 持久化原始 Prompt 并支持顶/底方向键回放；增强 `Shift+Enter`、`Ctrl+Enter`/`Ctrl+J` 换行；结束状态改为 TTFT 与生成阶段 TPS。
- 增加模型上下文窗口自动解析：交互启动时预热活动模型或全部自动路由候选模型，Provider/模型切换后立即解析并要求用户确认探测值，手动输入的窗口覆盖探测结果；支持 OpenAI/OpenRouter 扩展元数据、Ollama、llama.cpp、models.dev、独立缓存、自动路由有效窗口和三轮溢出压缩恢复；配置改为保留字段存在性的分层稀疏持久化。
- 增加内置 `todolist`：完整替换并校验 session 任务清单，类型化结果进入 Agent 上下文、受保护的 `Task state` 账本分类、压缩摘要和 schema v1 session 快照；`/new` 清空清单。
- TUI 增加双态任务树：请求运行时显示在 activity 行下方，完成或恢复后以状态摘要紧接最后一条历史内容并随 viewport 滚动。最多显示 5 个任务并用英文状态计数折叠其余项目；进行中项优先、completed 项后置。`todolist` 卡片从历史区隐藏，`/tasks` 与 `Ctrl-T` 保留全量清单和工具详情。
- Markdown 内联代码改为仅使用 Eylu 主题强调色文字，移除背景填充。
- 增加内置 `ask`：TUI 底部工作台支持 1 至 5 题、单选、多选、自定义答案、翻页、paste 与取消；`--no-tui` 文本 TTY 提供编号选择。Plan Agent 可提问，JSON、JSONL 与管道模式保持无阻塞。
- 工具执行器增加可选超时策略；普通工具继续使用 `tool_timeout_sec`，`ask` 直接跟随父请求 context，用户回答、取消或请求结束后释放等待通道。
- Provider API Key 改为与 `base_url` 同表的 `api_key` 明文字段；移除凭据引用、系统凭据库实现及对应依赖，配置 Key 继续从 JSON/session 状态中排除并参与日志脱敏。
- workspace 从配置 schema 迁移为 `--workspace > EYLU_WORKSPACE > cwd` 运行时上下文；新 session 将 OS、日期和 Git 状态快照注入 system prompt 并持久化，旧 session 首次恢复时自动补采并清除旧 DriverState。
- TUI 历史区增加按显示列拖选、跨 viewport 滚轮扩展、系统剪贴板自动复制与短时状态提示；选区稳定覆盖 ANSI/OSC 与中文宽字符。输入框增加 1 至 8 行动态高度及 `Shift+Enter`/`Ctrl+Enter` 换行，并统一修正原生光标坐标。
- 增加统一 `/` 与 `@` 补全面板、顶层 Skill 命令、Git-aware 文件候选，以及带边界校验和上下文预算的 Skill/文件引用注入。
- `Shift+Tab` 支持四模式循环与运行期排队；耗时按毫秒、秒、分钟自动格式化。
- `plan` 升级为继承当前模型与父上下文的隔离规划 Agent，只开放读取类工具和受分类器约束的 shell，仅将最终计划回写主会话并清除 DriverState；TUI 与静态入口共用同一 runner。
- Plan 完成后在保留历史可见的底部三分之一工作台增加 `Auto`、`Full`、`Reject` 执行入口与 `Tab` 修改意见循环，确认后切换权限模式并由主会话直接开始实现。
- 权限审批升级为 Eylu 底部工作台，展示工具动作、模型申请理由与策略依据；拒绝可附带反馈供模型调整，空理由拒绝会中断请求并输出带耗时的 `Interrupted after` 指标。
- Bubble Tea 界面采用 Eylu Signal 语义色板，统一输入、Markdown、工具活动、选择、高危提示与底部工作台的视觉层级。
- 根命令默认进入多轮 Chat，并支持直接传入 prompt 和全部 Chat 参数；`eylu chat` 保持兼容。
- 项目采用 Apache License 2.0，版权主体为 xnqycs。
- GoReleaser 的六个平台 tar.gz/zip 归档调整为仅包含对应的 `eylu` 或 `eylu.exe` 主程序，checksum 与 Sigstore 签名资产保持独立发布。
- 增加可复现的第三方声明生成器及 CI 漂移检查。

兼容性：protocol v1 与 session schema v1 保持不变。依赖：新增 `github.com/atotto/clipboard`。

## Phase 9 - 扩展生态与发布

- 增加按任务、能力、上下文窗口、优先级和估算成本选择 Provider 的确定性自动路由；支持 `--route`、`--task` 与 `--require-reasoning`。
- 增加首 token、总耗时、工具成功率、压缩次数、token usage 和估算成本指标，并将同一 request ID 传入工具审计。
- 在 Driver 声明并行能力时并发执行连续的显式只读工具，提供并发上限、稳定结果顺序、取消收敛和 panic 隔离。
- 基于官方 Go SDK 增加完整 MCP 客户端：支持 stdio、Streamable HTTP、SSE、OAuth、会话恢复、动态目录、tools/resources/prompts、roots/sampling/elicitation，以及 CLI/TUI 管理；环境变量按名称白名单转发，只读权限需本地显式配置。
- 增加 Ed25519 签名 Skill 仓库、包和目录双 SHA-256 校验、安装/更新/验签、user/project/team 范围与团队锁文件。
- 增加版本元数据、GoReleaser 六平台归档、SHA-256 checksums、Sigstore keyless 签名、三平台 CI 和发布工作流。

依赖：新增 `github.com/modelcontextprotocol/go-sdk` 和 `golang.org/x/mod`；发布链路使用 GoReleaser v2、Cosign、Staticcheck 与 actionlint。

## Phase 8 - 会话持久化

- 增加 append-only JSONL 事件日志、原子 snapshot、SHA-256 附件、尾部损坏修复和显式 schema 迁移。
- 增加 `--session`、`--resume` 与 `sessions list|show|delete|cleanup|migrate`，`/new` 会关闭旧 session 并持久化新边界。
- 持久化完整 transcript、Provider generation、权限模式、上下文账本、Skill digest 和 opaque DriverState；敏感凭据保持在会话文件之外。
- 增加 session 数量/容量清理策略、跨工作区校验、恢复时 Skill 重验证和远端 DriverState 失效处理。

## Phase 7 - 终端体验

- 增加 Bubble Tea v2 单写入 TUI、Bubbles v2 textarea/viewport/spinner、Lip Gloss v2 七类样式和 Glamour v2 Markdown。
- 增加稳定 header/history/loading/input/status 布局、滚动历史、工具摘要与分页详情、确认弹窗和取消状态。
- 增加 Provider 列表与 password 表单、模型拉取/刷新/筛选/选择/手工 ID、Skill 状态与名称补全、上下文进度及来源展开。
- 增加全部 operation state、operation ID 迟到事件过滤、150ms 状态过渡、重试倒计时和 resize 处理。
- 增加 `--no-animation`、`--no-tui`、`NO_COLOR`、`TERM=dumb`/管道降级与 `--output jsonl` 稳定事件流。

依赖：最低 Go 版本更新为 1.25.8；新增 `charm.land/bubbletea/v2`、`bubbles/v2`、`lipgloss/v2` 与 `glamour/v2`。

## Phase 6 - 上下文管理

- 增加同源 `PromptBuilder` 与全分类 `ContextLedger`，请求内容、工具 schema、DriverState 和 `/context` 使用同一组 blocks。
- 增加稳定项目地图、Skill catalog 分页、Skill/MCP 来源明细与 exact/estimated 标记。
- 增加上下文预算、完整 tool call/result 原子组压缩、结构化摘要和大工具结果模型片段。
- 完整 transcript 与压缩请求视图分离；已激活 Skill 正文继续作为 protected block 按 digest 去重。
- Responses DriverState 支持增量远端输入；端点拒绝 `previous_response_id` 时自动回退并记忆兼容能力。

配置：新增 token 近似比率、输出预留、最近轮次、项目地图、工具片段、Skill catalog 页和摘要上限；均支持 `EYLU_*` 环境变量覆盖。

兼容性：protocol v1 与配置 schema 版本保持不变；已有配置自动使用安全默认值。

## Phase 5 - Agent Skills

- 增加兼容 Agent Skills 规范的严格 frontmatter parser、固定目录发现、优先级、shadow/invalid 诊断。
- 增加项目级工作区信任、原子持久化、撤销和非 TTY 显式信任选项。
- 增加 catalog 渐进披露、`activate_skill`、protected digest 去重和 `read_skill_resource` 路径边界。
- 增加 `/skills`、`/skill` 与 `eylu skills list|show|validate|diagnose|trust|revoke`。
- Skill 脚本继续通过 `bash`、当前权限模式、超时、进程树和审计执行。

兼容性：仅在发现有效 Skill 时增加 catalog 和两个 Skill 工具；无 Skill 环境保持原请求结构。

## Phase 4 - 安全与四种权限模式

- 增加 `manual`、`plan`、`auto`、`full` 本地权限矩阵及 `--mode`、`/mode` 热切换。
- 增加链式命令分类、重定向/命令替换防绕过、白名单、阻止规则与高危模式。
- 高危操作支持多重确认和醒目 warning；Plan 模式将拒绝结果作为 tool result 回传模型。
- Windows 使用 Job Object、Unix 使用进程组，取消时回收整个命令子进程树。
- 工具审计增加模式、命令分类、确认次数、warning、耗时和退出码。

配置：新增命令策略列表和 `shell_environment` 白名单；默认权限模式保持 `manual`。

## Phase 3 - 精确编辑与项目探索

- 增加共享 `RepositoryIndex`，复用 Git NUL 文件索引与标准 ignore/exclude 语义。
- 增加 `search_code`、`list_directory` 和精确匹配 `edit_file`。
- `edit_file` 保留文件权限和 CRLF/LF 风格，原子写入前生成 unified diff 与增删统计。
- 普通目录或 Git 故障时使用受限文件系统遍历，继续保持稳定排序和符号链接边界。

兼容性：新增三个内置工具；protocol v1 与配置 schema 版本保持不变。

## Phase 2 - 工具调用闭环

- 增加 `ToolRegistry`、统一 `ToolExecutor`、基线权限检查、确认回调和结构化审计。
- 增加工作区受限的 `read_file`、原子 `write_file` 与跨平台 `bash` 工具。
- 增加显式 Agent Loop、多工具执行、call ID 配对、工具失败回传、迭代和 token 预算。
- Responses 与 Chat 驱动增加 tool call/result 双向映射。

配置：新增 `max_total_tokens` / `EYLU_MAX_TOTAL_TOKENS`；现有配置文件继续按默认值加载。

安全：shell 仅继承白名单环境；所有工具统一经过本地策略、超时、输出上限和审计。

## Phase 1 - 多轮流式会话

- 增加内存 transcript、session 边界、关闭会话快照和 Provider generation 感知。
- 增加 Responses SSE 与 Chat Completions 流式驱动，覆盖文本、usage、函数参数和断线语义。
- 增加 `/new`、`/context`、Provider/模型热管理和请求取消。
- 增加基础 ContextLedger，统一登记 system、Skill、MCP、工具、消息、摘要、driver state 与输出预留类别。

兼容性：配置 schema 与 protocol v1 保持不变；新增 adapter 名称 `openai_chat`。

## Phase 0 - 可运行骨架

- 初始化 Go module `Eylu`、Cobra CLI、protocol v1、ProviderManager 与 Provider 配置抽象。
- 增加 Provider CRUD、模型发现、首次引导、Responses 同步驱动与统一错误码。
- Provider API Key 随 Provider 配置持久化，敏感日志统一脱敏。

兼容性：初始配置 schema 版本为 1，初始领域协议版本为 1。
