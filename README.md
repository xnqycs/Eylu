# Eylu

Eylu 是一个面向本地代码库的 Go 终端编程 Agent。核心协议、模型驱动、工具、权限、上下文和会话持久化保持解耦，兼容 OpenAI Responses 风格的 HTTP 网关。

## 构建

Eylu 需要 Go 1.25.8 或更高版本。

```bash
go build -o eylu .
go test ./...
```

## 快速开始

使用环境变量临时覆盖 API Key，并通过运行时参数发起一次请求：

```powershell
$env:EYLU_API_KEY="your-key"
go run . "你好" --base-url "https://api.openai.com/v1" --model "your-model"
```

将 API Key 随 Provider 明文持久化到配置文件：

```powershell
go run . providers add work --base-url "https://api.openai.com/v1" --model "your-model" --api-key "your-key"
go run . providers models --provider work
go run . "检查当前项目" --provider work
```

`--api-key` 参数会进入 shell 历史；TUI 的 password input 可隐藏输入过程。两种入口都会将 Key 明文写入 `config.toml`。

Provider 可声明适用任务、优先级和每百万 token 成本。自动路由会先过滤 Driver 能力与上下文窗口，再按任务匹配、优先级、已知上下文标记、估算成本、上下文窗口和名称稳定排序：

```powershell
go run . providers add coding --base-url "https://api.openai.com/v1" --model "your-model" --api-key "your-key" --routing-task coding,debugging,testing --routing-priority 20 --input-cost 1.25 --output-cost 10
go run . "审查并测试这个项目" --route auto --task review --require-reasoning
```

`routing_mode = "fixed"` 保持活动 Provider；`routing_mode = "auto"` 允许每个请求选择 Provider。显式 `--provider` 固定本次请求。文本模式在 stderr 输出路由决策与请求指标；JSONL 模式输出 `routing` 和 `metrics` 事件，指标包含 request ID、首 token/总耗时、工具成功率、压缩次数、usage 与估算成本。

模型上下文窗口默认自动解析。交互程序启动时会预热活动模型，自动路由会并发预热全部候选模型；Provider 或模型切换成功后会立即解析新模型，无需先发送 prompt。用户选择模型后，界面会展示探测值与来源并要求确认；探测不正确时可输入正整数覆盖值。用户确认或输入的 `context_window` 优先于探测结果。Eylu 依次使用服务端模型元数据、Ollama `/api/ps`/`/api/show`、llama.cpp `/props`、models.dev 和内置 `256K → 8K` 阶梯；上下文溢出会触发最多三轮压缩重试并缓存服务确认的限制。`--catalog-provider` 可显式指定 models.dev Provider ID。解析缓存位于 `~/.eylu/state/model-metadata.json`，`/context` 展示配置值、探测值、有效值和来源。

兼容端点可在 Provider 中选择两种 adapter：

- `openai_responses`：使用 `/v1/responses` 和类型化 SSE 事件。
- `openai_chat`：使用 `/v1/chat/completions` 和 Chat Completions 流。

思考等级与 `base_url`、`model` 一起配置在 Provider 表中：

```toml
gradient_enabled = false

[providers.default]
adapter = "openai_responses"
base_url = "https://api.example.com/v1"
model = "gpt-5.6-sol"
reasoning_effort = "high"
```

统一等级为 `auto | low | medium | high | xhigh | max | ultra`。`auto` 使用 Provider 的模型默认值，Responses 请求省略 `reasoning.effort`，Chat Completions 请求省略顶层 `reasoning_effort`；其他等级原样发送。模型档案按规范化后的模型 ID 选择可用等级：

| 模型 | 可用等级 |
|---|---|
| `gpt-5.6-sol`、`gpt-5.6-terra` | `auto, low, medium, high, xhigh, max, ultra` |
| GPT/o-series `*-pro` | `auto, high` |
| `gpt-5.1-codex-max`、`gpt-5.2*` 至 `gpt-5.5*` | `auto, low, medium, high, xhigh` |
| 其他 GPT-5/Codex/o-series/GPT-OSS | `auto, low, medium, high` |
| Claude Opus 4.7+ | `auto, low, medium, high, xhigh, max` |
| 其他 Claude reasoning 模型 | `auto, high, max` |
| Gemini reasoning 模型 | `auto, low, high` |
| DeepSeek V4、GLM-5.2 | `auto, high, max` |
| Kimi K3 | `auto, max` |
| Qwen、其他 GLM/Kimi、MiniMax、DeepSeek R1 | `auto` |
| 未知或自定义模型 | `auto, low, medium, high` |

模型切换后，超出新档案范围的已存等级会在同一次配置更新中重置为 `auto` 并显示提示。档案设计参考 [Codex 模型目录](https://github.com/openai/codex/blob/main/codex-rs/core/models.json)、[OpenCode variants](https://opencode.ai/docs/models/)、[Hermes reasoning 配置](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/configuration.md) 和 [OpenClaw thinking profiles](https://docs.openclaw.ai/thinking)。

在终端直接运行 `go run .` 会进入多轮交互会话。可用命令包括 `/help`、`/new`、`/tasks`、`/context`、`/skills`、`/skill`、`/providers`、`/provider add|edit|delete|use`、`/model`、`/effort`、`/gradient`、`/mode` 与 `/quit`。文本输出实时呈现模型增量；`--output json` 输出完整响应对象。

兼容入口 `go run . chat [prompt]` 继续可用。prompt 与子命令同名时，可使用 `go run . -- "sessions"` 将其作为对话内容发送。

TTY 默认启动 Bubble Tea v2 全屏界面，启动历史顶部使用加宽粗体斜体字符画展示 Eylu、构建版本和当前工作目录；渐变默认关闭，启用后 Banner 与底部状态栏以约 20 FPS 使用主题强调色 `#35BDB2` 的明暗单色渐变逐字符流动。界面包含滚动历史、1 至 8 行动态输入框、Markdown、工具状态/详情、独立任务树、底部审批与提问工作台、Provider 表单、模型筛选、Skill 状态与上下文进度。底部状态栏与输入光标对齐，新启动或 `/new` 创建且尚未发送 Prompt 的会话显示 `Context 100% left · Context 0% used`；会话开始后按真实占用展示，未知窗口展示已统计 token，右侧使用随任务阶段变化的友好短句。请求运行时，任务树位于 activity 行与输入分隔线之间；请求完成或恢复 session 后，清单以 `3 tasks (0 done, 1 in progress, 2 open)` 摘要紧接最后一条历史内容，并随 viewport 滚动。两种状态最多展示 5 个任务，超出部分使用 `... +N pending`、`... +N completed` 等英文计数；进行中项优先，completed 项稳定移到末尾。`todolist` 工具卡从历史区隐藏，完整参数和结果仍可通过 `Ctrl-T` 查看；连续工具卡组成紧凑工具组，下一条普通消息前保留一行空白。`/tasks` 打开带 `[ ]`、`[>]`、`[x]`、`[-]` 状态标记的完整清单。历史区支持鼠标拖选并自动复制纯文本，拖选期间可用滚轮跨越当前 viewport，复制状态在输入框上方显示 2 秒；ANSI、OSC 链接、中文宽字符、软换行和滚动偏移均按显示列处理。界面采用 Eylu Signal 语义色板，焦点、工具活动、确认、风险与选区使用独立颜色；Markdown 内联代码只使用主题强调色文字，不绘制背景。`Enter` 提交，`Shift+Enter`、`Ctrl+Enter` 或兼容编码 `Ctrl+J` 换行，超过 8 行后输入框内部滚动。光标位于输入内容视觉顶端或底端时，`Up`/`Down` 会浏览当前 session 持久化的原始 Prompt，并在越过最新记录时恢复未发送草稿。

输入 `/` 会在输入框上方显示命令与英文说明，继续输入可按前缀筛选；`/mode`、`/provider`、`/skill` 提供上下文子选项，活跃 Skill 也可作为顶层命令使用。输入 `/effort` 会立即展开当前模型支持的等级，输入 `/gradient` 会展开 `On`、`Off`；两者均把光标定位到带绿色 `*` 的当前项。`Up`/`Down` 或 `Ctrl-P`/`Ctrl-N` 移动，`Enter` 选择并持久化，`Tab` 填入输入框后继续编辑，`Esc` 关闭。也可直接提交 `/effort high`、`/gradient on` 或 `/gradient off`；对应无参数命令在 `--no-tui` 中显示当前值和可用选项。右上角按 `provider  model  effort` 展示当前请求状态，窄终端优先保留 effort。输入 `@` 可引用活跃 Skill 或仓库文件，支持 `@index.html`、`@build/index.html` 与带引号路径；补全列表使用 Git 标准 ignore/exclude 语义，主动输入的完整路径或唯一文件名可引用 ignored 文件。提交时 Skill 自动激活，UTF-8 文件快照按配置预算注入请求。方向键、`Tab`、`Enter` 和 `Esc` 用于操作补全面板。

流式活动行展示阶段、自动换算后的耗时、每回合发送 token 和实时接收 token；reasoning delta 活跃时追加 `thinking`，进入文本或工具阶段后改为整秒的 `thought for 13s`。请求结束行展示总耗时、TTFT 与模型实际生成阶段的 TPS，例如 `Completed in 15.992s; TTFT 3.044s; TPS 42.6 t/s.`。TUI 会把 provider 发出的极小文本与工具参数 delta 合并成小批次，并缓存已完成的 Markdown 渲染，减少长会话重绘。模型生成 `bash`、`write_file`、`edit_file` 或其他需审批的工具参数时，必须提供面向用户的 `reason`；审批工作台同时展示动作摘要、申请理由和策略依据，仅提供单次同意与拒绝。拒绝时按 `Tab` 可填写反馈并回传模型继续当前请求；直接拒绝会中断请求并显示相同指标。模型调用 `ask` 时，工作台逐题展示 2 至 4 个选项和自定义答案入口；方向键切换问题，`Space` 勾选多选项，`Tab` 编辑自定义答案，`Enter` 提交，`Esc` 取消当前请求。`--no-tui` 的文本 TTY 使用编号、逗号分隔多选和 `o` 自定义输入。Provider 表单的 API Key 使用 password input，并明文写入 Provider 配置；模型面板支持刷新、筛选、选择和手工 ID。`Shift+Tab` 按 `manual → plan → auto → full` 循环模式；运行期间的切换在下一轮生效。`Ctrl-C` 在请求期间第一次取消，第二次退出；`Ctrl-T` 打开最近工具详情。

```powershell
go run . --no-animation
go run . --no-tui
go run . "检查项目" --provider work --output jsonl
```

启用渐变时，`--no-animation` 使用固定语义色并保留静态状态与耗时；`TERM=dumb`、管道和结构化输出使用静态路径；`NO_COLOR` 会移除 ANSI 颜色。`--output jsonl` 逐行输出 context、模型事件、工具审计和最终响应，便于脚本消费。

工作区属于运行时上下文，按 `--workspace`、`EYLU_WORKSPACE`、当前目录的顺序解析。TOML 配置中的遗留 `workspace` 键会被忽略，并在下一次配置保存时移除。

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

`/new` 会先刷新当前快照并追加关闭事件，再创建空 session；历史记录仍可通过 `--session <id>` 恢复。原始 Prompt 与任务清单均随 session 保存和恢复，新 session 从空状态开始；旧 v1 session 首次恢复时会从 user turns 回填 Prompt 历史。每个新 session 会采集工作目录、平台、OS 版本、日期以及 Git 分支、状态和最近五条提交，并将其作为会话快照保存。恢复时复用原快照；旧 session 首次恢复时补采一次。删除操作在终端中确认，脚本环境使用 `sessions delete <id> --yes`。

默认状态目录为 `~/.eylu/state/sessions`，`EYLU_STATE_DIR` 可修改其父目录。每个 session 使用 `events.jsonl` 作为事实日志，`snapshot.json` 加速恢复；超过 16 KiB 的工具输出按 SHA-256 保存为附件并在加载时校验。恢复 Skill 时会重新读取正文并复核保存的 digest。schema 版本不兼容时需显式运行迁移命令，迁移前会保留 v0 备份。

Agent 默认提供以下工具：

- `read_file`：读取工作区内 UTF-8 文件，限制读取字节并拒绝路径穿越。
- `write_file`：在确认后原子创建或替换文件，父目录创建必须由模型显式声明。
- `bash`：在确认后通过平台 shell 执行命令，限制环境、超时与输出大小。
- `edit_file`：按预期匹配次数精确替换，保留权限与换行风格并返回 unified diff。
- `search_code`：在 Git ignore 语义下进行字面量或 RE2 正则搜索。
- `list_directory`：从共享仓库索引生成稳定、限深的目录树。
- `todolist`：完整替换 session 内最多 20 项的有序任务清单，并返回类型化进度；执行模式可用。
- `ask`：暂停当前回合并收集 1 至 5 个单选或多选问题；TUI、文本 TTY 与 Plan Agent 可用，且禁止索取密码、API Key、令牌等敏感信息。

JSON、JSONL 与管道运行只注册 `todolist`，避免在无交互客户端中阻塞等待回答。两个 session 工具保持顺序执行，并由本地策略自动放行。

Driver 声明并行工具能力时，连续的 `read_file`、`search_code`、`list_directory`、`read_skill_resource` 和显式只读 MCP 工具可按 `max_parallel_tools` 并发执行；结果与事件继续使用模型调用顺序。

只读工具直接执行；写入与命令工具会在 TTY 中确认。脚本化运行可使用 `--yes` 明确授权本次请求中的确认项：

```powershell
go run . "读取 go.mod 并执行 go test ./..." --provider work --yes
```

权限模式可通过 `--mode` 或交互命令 `/mode` 切换：

| 模式 | 行为 |
|---|---|
| `manual` | 读取自动执行；写入和命令确认；高危操作确认两次。 |
| `plan` | 在继承当前模型、主会话、Skill、项目地图与 MCP 上下文的隔离规划 Agent 中执行；开放读取工具、`ask` 和已分类的只读命令，仅将最终计划回写主会话。 |
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

Eylu 使用同一个 `PromptBuilder` 生成模型请求和 `ContextLedger`。`/context` 默认使用横向 Signal Strip 展示输入、输出预留和剩余空间，并聚合为 System、Conversation、Tools、Skills、MCP、Model state 与 Other；按 `Enter` 展开 system prompt、受保护的 Task state、Skill catalog/正文/资源、MCP、工具 schema、user/agent 消息、工具结果、项目地图、摘要、DriverState、输出预留及来源详情。system prompt 包含保存的会话环境和 Git 快照，model ID 按实际请求模型渲染；当前任务清单以结构化系统块注入，压缩摘要中的未完成任务来自真实的 pending/in_progress 项。压缩记录和最近一次 Provider usage 在详情中独立显示。

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

常规配置优先级为命令行参数、`EYLU_*` 环境变量、工作区 `.eylu/config.toml`、用户目录 `~/.eylu/config.toml`、默认值。所有默认值由代码提供，TOML 只保存用户显式字段；显式的默认值、`false`、`0` 和空数组会稳定保留。命令只更新当前配置层，继承值与环境变量保持在来源层。`[model_metadata]` 默认省略，用户可按字段覆盖探测超时、TTL、目录 URL、缓存限制和阶梯。workspace 使用独立的运行时优先级 `--workspace > EYLU_WORKSPACE > cwd`。Provider 的 `api_key` 与 `base_url` 位于同一个 TOML 表并以明文保存；应限制配置文件仅供当前系统用户读取。`EYLU_API_KEY` 可临时覆盖所有 Provider 的配置值。

session 保存完整 Eylu transcript、环境快照、上下文账本、权限模式、Provider generation、模型引用、有效思考等级、最近一次探测限制和 opaque DriverState；API Key 与 Provider headers 不进入 session 文件。恢复请求会重新解析模型限制。旧 session 补采环境时会清除原 DriverState，并使用本地 transcript 重建模型请求。`max_sessions` 与 `max_session_bytes` 控制自动清理上限，`sessions cleanup` 可立即执行清理。

## 发布

`eylu version` 显示 version、commit、date 与构建器。GoReleaser 为 Linux、Windows、macOS 的 amd64/arm64 生成归档和 `Eylu_<version>_checksums.txt`；每个平台归档仅包含 `eylu` 或 `eylu.exe` 主程序；tag 工作流使用 Sigstore keyless 对 checksum 文件生成 `.sigstore.json` bundle。

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=sign
```

CI 在 Linux、Windows、macOS 执行测试、vet、原生构建和 smoke test；Linux 质量任务额外执行 race detector、格式检查、第三方声明漂移检查、GitHub Actions 检查与 Staticcheck。发布 tag 使用严格 SemVer 格式。

稳定版使用 `vMAJOR.MINOR.PATCH` 标签，预览版在版本号后增加 SemVer 预发布标识。发布工作流只接受 main 分支历史中的标签；它会先复用完整 CI，再创建草稿 Release、上传全部产物，并在公开仓库中生成构建来源证明，最后公开 Release。`rc`、`beta`、`alpha` 等预发布标签会自动显示为 GitHub Pre-release：

```bash
# 稳定版
git tag -a v1.1.0 -m "Release v1.1.0"
git push origin v1.1.0

# 发布候选版
git tag -a v1.1.0-rc.1 -m "Release v1.1.0-rc.1"
git push origin v1.1.0-rc.1

# Beta 或 Alpha 版
git tag -a v1.1.0-beta.1 -m "Release v1.1.0-beta.1"
git push origin v1.1.0-beta.1
```

同一版本按 `alpha.1 → beta.1 → rc.1 → v1.1.0` 递进。标签推送错误且工作流尚未成功发布时，可先在 GitHub 删除对应草稿 Release，再删除远端标签并创建正确标签。

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
