# Changelog

## Unreleased

- TUI 历史区增加按显示列拖选、跨 viewport 滚轮扩展、系统剪贴板自动复制与短时状态提示；选区稳定覆盖 ANSI/OSC 与中文宽字符。输入框增加 1 至 8 行动态高度及 `Shift+Enter`/`Ctrl+Enter` 换行，并统一修正原生光标坐标。
- 增加统一 `/` 与 `@` 补全面板、顶层 Skill 命令、Git-aware 文件候选，以及带边界校验和上下文预算的 Skill/文件引用注入。
- `Shift+Tab` 支持四模式循环与运行期排队；耗时按毫秒、秒、分钟自动格式化。
- `plan` 升级为继承当前模型与父上下文的隔离规划 Agent，只开放读取类工具和受分类器约束的 shell，仅将最终计划回写主会话并清除 DriverState；TUI 与静态入口共用同一 runner。
- Plan 完成后在保留历史可见的底部三分之一工作台增加 `Auto`、`Full`、`Reject` 执行入口与 `Tab` 修改意见循环，确认后切换权限模式并由主会话直接开始实现。
- 权限审批升级为 Eylu 底部工作台，展示工具动作、模型申请理由与策略依据；拒绝可附带反馈供模型调整，空理由拒绝会中断请求并输出带耗时的 `Interrupted after` 指标。
- Bubble Tea 界面采用 Eylu Signal 语义色板，统一输入、Markdown、工具活动、选择、高危提示与底部工作台的视觉层级。
- 根命令默认进入多轮 Chat，并支持直接传入 prompt 和全部 Chat 参数；`eylu chat` 保持兼容。
- 项目采用 Apache License 2.0，版权主体为 xnqycs。
- 发布归档增加项目 NOTICE、许可证和覆盖六个目标平台的第三方许可证全文。
- 增加可复现的第三方声明生成器及 CI 漂移检查。

兼容性：protocol v1 与 session schema v1 保持不变。依赖：新增 `github.com/atotto/clipboard`。

## Phase 9 - 扩展生态与发布

- 增加按任务、能力、上下文窗口、优先级和估算成本选择 Provider 的确定性自动路由；支持 `--route`、`--task` 与 `--require-reasoning`。
- 增加首 token、总耗时、工具成功率、压缩次数、token usage 和估算成本指标，并将同一 request ID 传入工具审计。
- 在 Driver 声明并行能力时并发执行连续的显式只读工具，提供并发上限、稳定结果顺序、取消收敛和 panic 隔离。
- 基于官方 Go SDK 增加 MCP stdio 客户端，接入 instructions、tools 与 resources；环境变量按名称白名单转发，只读权限需本地显式配置。
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

- 初始化 Go module `Eylu`、Cobra CLI、protocol v1、ProviderManager 与凭据抽象。
- 增加 Provider CRUD、模型发现、首次引导、Responses 同步驱动与统一错误码。
- 配置文件只保存凭据引用，敏感日志统一脱敏。

兼容性：初始配置 schema 版本为 1，初始领域协议版本为 1。
