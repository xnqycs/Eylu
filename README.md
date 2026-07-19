# Eylu

Eylu 是一个面向本地代码库的 Go 终端编程 Agent。核心协议、模型驱动、工具、权限、上下文和会话持久化保持解耦，兼容 OpenAI Responses 风格的 HTTP 网关。

## 构建

Eylu 需要 Go 1.25.8 或更高版本。

```bash
go build -o eylu .
go test ./...
```

## 快速开始

使用环境变量保存凭据，并通过运行时参数发起一次请求：

```powershell
$env:EYLU_API_KEY="your-key"
go run . "你好" --base-url "https://api.openai.com/v1" --model "your-model"
```

持久化 Provider 配置：

```powershell
$env:EYLU_API_KEY="your-key"
go run . providers add work --base-url "https://api.openai.com/v1" --model "your-model" --credential-type env --credential-env EYLU_API_KEY
go run . providers models --provider work
go run . "检查当前项目" --provider work
```

Provider 可声明适用任务、优先级和每百万 token 成本。自动路由会先过滤 Driver 能力与上下文窗口，再按任务匹配、优先级、已知上下文标记、估算成本、上下文窗口和名称稳定排序：

```powershell
go run . providers add coding --base-url "https://api.openai.com/v1" --model "your-model" --credential-type env --credential-env EYLU_API_KEY --routing-task coding,debugging,testing --routing-priority 20 --input-cost 1.25 --output-cost 10
go run . "审查并测试这个项目" --route auto --task review --require-reasoning
```

`routing_mode = "fixed"` 保持活动 Provider；`routing_mode = "auto"` 允许每个请求选择 Provider。显式 `--provider` 固定本次请求。文本模式在 stderr 输出路由决策与请求指标；JSONL 模式输出 `routing` 和 `metrics` 事件，指标包含 request ID、首 token/总耗时、工具成功率、压缩次数、usage 与估算成本。

兼容端点可在 Provider 中选择两种 adapter：

- `openai_responses`：使用 `/v1/responses` 和类型化 SSE 事件。
- `openai_chat`：使用 `/v1/chat/completions` 和 Chat Completions 流。

在终端直接运行 `go run .` 会进入多轮交互会话。可用命令包括 `/help`、`/new`、`/context`、`/skills`、`/skill`、`/providers`、`/provider add|edit|delete|use`、`/model`、`/mode` 与 `/quit`。文本输出实时呈现模型增量；`--output json` 输出完整响应对象。

兼容入口 `go run . chat [prompt]` 继续可用。prompt 与子命令同名时，可使用 `go run . -- "sessions"` 将其作为对话内容发送。

TTY 默认启动 Bubble Tea v2 全屏界面，包含滚动历史、1 至 8 行动态输入框、Markdown、工具状态/详情、底部审批工作台、Provider 表单、模型筛选、Skill 状态与上下文进度。历史区支持鼠标拖选并自动复制纯文本，拖选期间可用滚轮跨越当前 viewport，复制状态在输入框上方显示 2 秒；ANSI、OSC 链接、中文宽字符、软换行和滚动偏移均按显示列处理。界面采用 Eylu Signal 语义色板，焦点、工具活动、确认、风险与选区使用独立颜色。`Enter` 提交，`Shift+Enter` 或 `Ctrl+Enter` 换行，超过 8 行后输入框内部滚动。

输入 `/` 会在输入框上方显示命令与英文说明，继续输入可按前缀筛选；`/mode`、`/provider`、`/skill` 提供上下文子选项，活跃 Skill 也可作为顶层命令使用。输入 `@` 可引用活跃 Skill 或仓库文件，文件列表使用 Git 标准 ignore/exclude 语义；提交时 Skill 自动激活，UTF-8 文件快照按配置预算注入请求。方向键、`Tab`、`Enter` 和 `Esc` 用于操作补全面板。

流式活动行展示阶段、自动换算后的耗时、每回合发送 token、实时接收 token 和 thinking 状态；Responses reasoning summary delta 与 Chat `reasoning_content` 用于实时估算，Provider usage 到达后校正为精确累计值。TUI 会把 provider 发出的极小文本与工具参数 delta 合并成小批次，并缓存已完成的 Markdown 渲染，减少长会话重绘。模型生成 `bash`、`write_file`、`edit_file` 或其他需审批的工具参数时，必须提供面向用户的 `reason`；审批工作台同时展示动作摘要、申请理由和策略依据，仅提供单次同意与拒绝。拒绝时按 `Tab` 可填写反馈并回传模型继续当前请求；直接拒绝会中断请求并显示 `Interrupted after <duration>` 指标。Provider 表单的 API Key 使用 password input；模型面板支持刷新、筛选、选择和手工 ID。`Shift+Tab` 按 `manual → plan → auto → full` 循环模式；运行期间的切换在下一轮生效。`Ctrl-C` 在请求期间第一次取消，第二次退出；`Ctrl-T` 打开最近工具详情。

```powershell
go run . --no-animation
go run . --no-tui
go run . "检查项目" --provider work --output jsonl
```

`--no-animation` 保留静态状态与耗时；`TERM=dumb`、管道和结构化输出使用静态路径；`NO_COLOR` 会移除 ANSI 颜色。`--output jsonl` 逐行输出 context、模型事件、工具审计和最终响应，便于脚本消费。

## 会话恢复

每次 chat 都会创建持久化 session。可以指定稳定 ID，或恢复当前工作区最近使用的 session：

```powershell
go run . "检查项目" --session review-1 --provider work
go run . "继续处理" --resume --provider work
go run . sessions list
go run . sessions show review-1 --output json
go run . sessions delete review-1
go run . sessions migrate review-1
```

`/new` 会先刷新当前快照并追加关闭事件，再创建空 session；历史记录仍可通过 `--session <id>` 恢复。删除操作在终端中确认，脚本环境使用 `sessions delete <id> --yes`。

默认状态目录为 `~/.eylu/state/sessions`，`EYLU_STATE_DIR` 可修改其父目录。每个 session 使用 `events.jsonl` 作为事实日志，`snapshot.json` 加速恢复；超过 16 KiB 的工具输出按 SHA-256 保存为附件并在加载时校验。恢复 Skill 时会重新读取正文并复核保存的 digest。schema 版本不兼容时需显式运行迁移命令，迁移前会保留 v0 备份。

Agent 默认提供以下工具：

- `read_file`：读取工作区内 UTF-8 文件，限制读取字节并拒绝路径穿越。
- `write_file`：在确认后原子创建或替换文件，父目录创建必须由模型显式声明。
- `bash`：在确认后通过平台 shell 执行命令，限制环境、超时与输出大小。
- `edit_file`：按预期匹配次数精确替换，保留权限与换行风格并返回 unified diff。
- `search_code`：在 Git ignore 语义下进行字面量或 RE2 正则搜索。
- `list_directory`：从共享仓库索引生成稳定、限深的目录树。

Driver 声明并行工具能力时，连续的 `read_file`、`search_code`、`list_directory`、`read_skill_resource` 和显式只读 MCP 工具可按 `max_parallel_tools` 并发执行；结果与事件继续使用模型调用顺序。

只读工具直接执行；写入与命令工具会在 TTY 中确认。脚本化运行可使用 `--yes` 明确授权本次请求中的确认项：

```powershell
go run . "读取 go.mod 并执行 go test ./..." --provider work --yes
```

权限模式可通过 `--mode` 或交互命令 `/mode` 切换：

| 模式 | 行为 |
|---|---|
| `manual` | 读取自动执行；写入和命令确认；高危操作确认两次。 |
| `plan` | 在继承当前模型、主会话、Skill、项目地图与 MCP 上下文的隔离规划 Agent 中执行；开放读取工具和已分类的只读命令，仅将最终计划回写主会话。 |
| `auto` | 写入与命令白名单自动执行；未知命令确认；高危操作确认两次。 |
| `full` | 普通操作自动执行；高危操作显示警告并确认。 |

命令分类会检查链式命令、重定向、命令替换、阻止规则和高危模式。`read_only_commands`、`auto_allow_commands`、`dangerous_commands`、`blocked_commands` 与 `shell_environment` 可在 TOML 中配置。

TUI 与 `--no-tui --mode plan` 使用同一 Plan runner。规划过程的 reasoning 与工具事件实时输出，中间 turns 保存在临时侧链；成功后主会话记录用户请求和最终计划并清除旧 DriverState，取消或失败时保持父会话 transcript 不变。TUI 随后在底部约三分之一高度的工作台显示执行入口，历史区继续展示完整计划：`Auto` 使用自动审批策略开始实现，`Full` 使用完整权限策略开始实现，`Reject` 退出 Plan 并回到 `manual`；按 `Tab` 可提交修改理由，由 Plan Agent 基于主会话中的现有计划重新规划并再次显示入口。

## Agent Skills

Eylu 默认扫描以下目录，并按项目级高于用户级、Eylu 原生目录高于跨客户端目录的顺序处理同名 Skill：

```text
<workspace>/.eylu/skills
<workspace>/.agents/skills
~/.eylu/skills
~/.agents/skills
```

项目级 Skill 需要工作区信任。TTY 会显示规范化路径并确认；脚本化运行使用 `--trust-workspace-skills`，也可通过 `eylu skills trust|revoke` 管理。启动上下文只加载名称、描述和来源；模型调用 `activate_skill` 后加载 protected 正文，再按需调用 `read_skill_resource`。

```powershell
go run . skills list
go run . skills show code-review
go run . skills validate ".agents/skills/code-review"
go run . skills diagnose --output json
```

交互会话支持 `/skills`、`/skill <name>`、顶层 `/<skill-name>` 和 `@skill:<name>`。Skill 的 `allowed-tools` 仅作为提示和审计信息，工具执行继续服从当前权限模式。

签名 Skill 仓库使用 Ed25519 公钥建立信任。索引和包地址接受 HTTPS，测试环境可使用 loopback HTTP；私有仓库的 Bearer token 通过环境变量名称引用：

```powershell
go run . skills registries add official --index-url "https://skills.example.com/index.json" --public-key "release=<base64-ed25519-public-key>" --token-env EYLU_SKILL_TOKEN
go run . skills remote official
go run . skills install official/code-review --scope user
go run . skills update code-review --scope user
go run . skills verify code-review --scope user
```

安装前会校验索引签名、ZIP SHA-256、解压后目录 SHA-256、路径边界、文件数量和大小，再通过稳定 staging 原子替换。签名载荷固定为 `eylu-skill-v1`、名称、规范化版本、解析后的绝对 package URL、包摘要、目录摘要和 key ID，各字段以 LF 连接。`project` 安装到 `.eylu/skills`，`team` 安装到 `.agents/skills` 并更新可移植的 `.eylu/skills.lock.json`；替换未受管理的目录需要显式 `--force`。

## MCP

MCP server 通过 stdio 子进程接入。下面的 TOML 配置只转发列出的环境变量，工作目录必须位于当前 workspace 内；`read_only_tools` 是本地安全授权，server annotation 只参与展示：

```toml
[mcp_servers.repository]
command = "repo-mcp"
args = ["serve", "--stdio"]
environment = ["REPO_MCP_TOKEN"]
working_directory = "."
read_only_tools = ["search", "inspect"]
timeout_seconds = 30
```

```powershell
go run . mcp list
go run . mcp inspect repository --output json
go run . "使用 repository 工具检查项目"
```

MCP instructions、tool schema、resource 内容和 tool result 分别进入 `ContextLedger`；server 配置或能力指纹变化会清除不再兼容的 DriverState。命令直接启动且不经过 shell，关闭 session、`/new` 和程序退出时会关闭子进程。

## 上下文管理

Eylu 使用同一个 `PromptBuilder` 生成模型请求和 `ContextLedger`，`/context` 会在一张表中列出 system prompt、Skill catalog/正文/资源、MCP、工具 schema、user/agent 消息、工具结果、项目地图、摘要、DriverState 与输出预留。Skill 和 MCP 类别会按来源展开；最近一次 Provider usage 独立显示。

已知上下文窗口达到上限时，Eylu 保留 system prompt、项目地图、当前用户目标、最近完整轮次和已激活 Skill，按完整的 tool call/result 原子组压缩较早内容。结构化摘要持续记录目标、完成修改、未完成任务、失败尝试、验证结果与 Skill digest；完整 transcript 仍保留在会话中。大工具结果进入模型前使用有头尾的受限片段，原始会话结果保持不变。

上下文参数可写入 TOML，也支持同名 `EYLU_*` 环境变量：

```toml
token_bytes_per_token = 4
reserved_output_tokens = 8192
context_recent_rounds = 3
max_project_map_bytes = 32768
max_tool_context_bytes = 8192
skill_catalog_page_bytes = 8192
max_summary_bytes = 16384
max_sessions = 100
max_session_bytes = 536870912
max_parallel_tools = 4
```

项目地图稳定登记受限文件树、语言统计、入口、配置和最近修改文件。Responses 驱动在端点支持时使用远端 response state 减少重复传输；HTTP 网关拒绝该能力时会自动记忆并切换到完整上下文请求。

配置优先级为命令行参数、`EYLU_*` 环境变量、工作区 `.eylu/config.toml`、用户目录 `~/.eylu/config.toml`、默认值。配置文件仅保存凭据引用；交互式首次引导会优先保存到系统 keyring。

session 保存完整 Eylu transcript、上下文账本、权限模式、Provider generation、模型引用和 opaque DriverState；API Key 与 Provider headers 不进入 session 文件。清除 DriverState 后仍可使用本地 transcript 重建模型请求。`max_sessions` 与 `max_session_bytes` 控制自动清理上限，`sessions cleanup` 可立即执行清理。

## 发布

`eylu version` 显示 version、commit、date 与构建器。GoReleaser 为 Linux、Windows、macOS 的 amd64/arm64 生成归档和 `Eylu_<version>_checksums.txt`；归档包含项目许可证、NOTICE 和第三方许可证全文；tag 工作流使用 Sigstore keyless 对 checksum 文件生成 `.sigstore.json` bundle。

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=sign
```

CI 在 Linux、Windows、macOS 执行测试、vet、原生构建和 smoke test；Linux 质量任务额外执行 race detector、格式检查、第三方声明漂移检查与 Staticcheck。发布 tag 使用 `v*` 格式。

## 许可证

Eylu 由 xnqycs 以 [Apache License 2.0](LICENSE) 发布。第三方组件及其适用条款见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 开发质量门槛

```bash
gofmt -l .
go mod verify
go vet ./...
go test ./...
go test -race ./...
go run ./scripts/generate-third-party-notices -check
staticcheck ./...
actionlint
```
