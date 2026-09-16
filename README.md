# Eylu

简体中文 | [English](README_EN.md)

面向本地代码库的终端编程 Agent。Eylu 在你的工作区中理解代码、调用工具、执行计划并保存会话，同时兼容 OpenAI Responses 与 Chat Completions 风格的 HTTP 网关。

[下载](https://github.com/xnqycs/Eylu/releases) · [更新日志](CHANGELOG.md) · [发版指南](RELEASING.md) · [License](LICENSE)

<p align="center">
  <img src="docs/assets/eylu-tui.png" alt="Eylu TUI 启动界面" width="1100">
</p>

## 为什么选择 Eylu

| 能力 | 使用体验 |
|---|---|
| 本地代码库上下文 | 自动采集项目结构、Git 状态与相关文件，引用内容受工作区边界保护 |
| 完整 Agent 循环 | 支持模型流式输出、多轮工具调用、任务清单、提问与执行审计 |
| 可控的工具权限 | `manual`、`plan`、`auto`、`full` 四种模式覆盖审阅、规划和自动执行 |
| 长会话管理 | 持久化会话、Prompt 历史、任务状态和上下文账本，支持压缩与恢复 |
| 多 Provider 路由 | 按任务、能力、上下文窗口、优先级和成本选择模型 |
| 可扩展能力 | 支持 Agent Skills、签名 Skill 仓库以及 MCP stdio、Streamable HTTP、SSE server |

Eylu 提供全屏 TUI，也能以纯文本、JSON 或 JSONL 方式接入脚本和自动化流程。

## 安装

### 下载预编译版本

从 [GitHub Releases](https://github.com/xnqycs/Eylu/releases) 下载与系统匹配的归档：

| 系统 | x64 | ARM64 |
|---|---|---|
| Windows | `Eylu_<version>_Windows_amd64.zip` | `Eylu_<version>_Windows_arm64.zip` |
| Linux | `Eylu_<version>_Linux_amd64.tar.gz` | `Eylu_<version>_Linux_arm64.tar.gz` |
| macOS | `Eylu_<version>_Darwin_amd64.tar.gz` | `Eylu_<version>_Darwin_arm64.tar.gz` |

解压后检查版本：

```powershell
# Windows
.\eylu.exe version
```

```bash
# Linux / macOS
chmod +x eylu
./eylu version
```

后续示例假设 `eylu` 已在 `PATH` 中。Windows 可将解压目录加入用户 `Path`；Linux 和 macOS 可将程序安装到用户命令目录：

```bash
mkdir -p "$HOME/.local/bin"
install -m 755 eylu "$HOME/.local/bin/eylu"
```

确保 `$HOME/.local/bin` 已加入 `PATH`。

每个归档只包含主程序。Release 同时提供 SHA-256 校验文件和 Sigstore bundle，验证方法见 [发版指南](RELEASING.md#5-发布后验证)。

### 从源码构建

需要 Go 1.25.8 或更高版本：

```bash
git clone https://github.com/xnqycs/Eylu.git
cd Eylu
go build -trimpath -o eylu .
go test ./...
```

## 快速开始

### 1. 启动 TUI

进入需要处理的项目目录，直接运行 Eylu：

```bash
cd path/to/your-project
eylu
```

首次启动会先显示 Provider 配置引导：

1. 确认 Provider 名称和 API Base URL。
2. 输入 API Key，输入过程会隐藏字符。
3. 从自动发现的模型中选择，或手动填写模型 ID。
4. 确认模型上下文窗口。

配置完成后，Eylu 会自动进入全屏 TUI。后续启动会直接使用已保存的 Provider。Provider 和 API Key 会保存到 `~/.eylu/config.toml`。

### 2. 开始对话

在底部输入框描述任务并按 `Enter`。Eylu 会读取当前工作区上下文，展示工具执行、任务进度和上下文用量。

常用交互命令：

```text
/help       查看命令
/new        创建新会话
/tasks      查看完整任务清单
/context    查看上下文使用情况
/providers  管理 Provider
/model      切换模型
/effort     调整思考等级
/skills     查看 Skills
/mode       切换权限模式
/quit       退出
```

### 环境变量与命令行配置

环境变量适合临时凭据和自动化环境：

```powershell
# Windows PowerShell
$env:EYLU_API_KEY="your-api-key"
```

```bash
# Linux / macOS
export EYLU_API_KEY="your-api-key"
```

提前创建 Responses Provider：

```bash
eylu providers add work --base-url "https://api.example.com/v1" --model "your-model-id"
eylu providers list
```

Chat Completions 兼容网关需要指定 adapter：

```bash
eylu providers add work-chat --adapter openai_chat --base-url "https://api.example.com/v1" --model "your-model-id"
```

配置完成后运行 `eylu` 进入 TUI。`EYLU_API_KEY` 会在请求时覆盖 Provider 中保存的 Key。

### 托管 Web Search 与 Web Fetch

Eylu 将 `web_search`、`web_fetch` 与普通 function tool 分开建模，并根据 `catalog_provider + adapter + model` 解析能力。已识别且支持 Web 能力的 Provider 会自动发布可执行工具；兼容网关可通过 `web_capabilities` 显式声明能力。Web 权限默认是 `allow`，搜索和抓取可直接执行；显式设置 `ask` 或 `deny` 可恢复确认或禁用策略。兼容 Responses 中转上的 GPT 模型会把同轮 `queries` 交给 Eylu 受控扇出，单批最多 10 条并发查询，完成后按原查询顺序归并为一个工具结果；`max_uses` 仍按模型发起的工具调用计数。TUI 会把批量查询拆成独立子项，并展示实际搜索词、打开的 URL 和来源；折叠态最多保留最新 5 项，`▸ … +N hidden` 显示隐藏数量，点击该行可展开或收起完整记录。

| Adapter | 原生 Web 映射 |
|---|---|
| `openai_responses` | Responses hosted search/fetch；支持 OpenAI、xAI 与 OpenRouter 方言 |
| `openai_chat` | Chat hosted search；支持 OpenRouter、Groq Compound 与 Qwen/DashScope 选项 |
| `anthropic_messages` | 版本化 server tools，并在单次请求内处理 `pause_turn` 续接 |
| `gemini_interactions` | `google_search` 与 `url_context` |
| `mistral_conversations` | standard/premium Web search |
| `perplexity_agent` | `web_search` 与 `fetch_url` |

核心 CLI 配置示例：

```bash
eylu providers edit work --catalog-provider openai --web-permission allow --web-search auto --web-fetch auto --web-max-uses 5 --web-context-size medium
```

完整 TOML 示例包含 hosted 能力覆盖、delegated fallback 和 MCP client fallback：

```toml
[providers.work.web_tools]
permission = "allow"

[providers.work.web_tools.search]
enabled = true
execution = "auto"
fallback = "delegated"
delegated_provider = "web-backup"
allowed_domains = ["example.com"]
blocked_domains = ["private.example.com"]
max_uses = 5
context_size = "medium"

[providers.work.web_tools.fetch]
enabled = true
execution = "client"
client_tool = "mcp__web__fetch"
trusted_network_boundary = true
max_uses = 3

[providers.work.web_capabilities]
hosted_web_search = true
hosted_web_fetch = true
hosted_tool_streaming = true
hosted_and_function_tools = true
search_domain_filter = true
search_location = true
search_usage_details = true
```

`execution` 支持 `auto`、`hosted`、`delegated`、`client`。`auto` 优先当前模型的 hosted 能力，并按显式 `fallback` 转入另一个已配置 Provider 或指定 MCP 工具。MCP fetch 必须设置 `trusted_network_boundary = true`；Eylu 会校验初始 HTTP(S) URL、凭据、域名规则及解析后的公网地址，MCP server 负责在可信边界内对重定向和后续 DNS 解析执行同等检查。

Hosted 会把查询、URL、域名规则、位置和允许的 Provider 选项发送给当前 Provider；delegated 会发送给目标 Provider；client 会把 canonical `query` 或 `url` 发送给指定 MCP server。Web 内容统一标记为不可信输入。活动、引用、Web token、费用和 backend 会进入协议事件、会话、JSON/JSONL、指标与审计记录。日志继续应用现有凭据脱敏。

## 常用工作流

### 单次请求

```bash
eylu --no-tui "检查当前项目并给出风险清单"
```

### 按 ID 恢复会话

```bash
eylu --resume auth-review
eylu chat --resume auth-review
```

`--resume <session-id>` 精确加载当前工作区中已存在的会话；ID 无效、缺失、损坏或属于其他工作区时返回非零退出码，会话存储保持原样。TUI 和 `--no-tui` 交互模式会显示已恢复的消息与工具历史并定位到最新内容；带 prompt 的一次性调用继续只输出本轮结果。交互式文本会话退出后会打印可直接执行的恢复命令。

`--session <id>` 保留“打开已有会话或按 ID 创建会话”的用途：

```bash
eylu "审查认证模块" --session auth-review
eylu --resume auth-review "继续修复"
eylu sessions list
eylu sessions show auth-review --output json
```

### 脚本化输出

```bash
eylu --no-tui --output jsonl "检查项目并运行测试"
```

JSONL 会逐行输出路由、上下文、模型、工具审计和最终响应事件，适合日志采集与自动化消费。

### 自动选择 Provider

为 Provider 声明任务和优先级：

```bash
eylu providers add coding --base-url "https://api.example.com/v1" --model "coding-model" --routing-task coding,debugging,testing --routing-priority 20
```

发起自动路由请求：

```bash
eylu --route auto --task review "审查本次修改并运行测试"
```

路由器会综合任务匹配、模型能力、有效上下文窗口、优先级和已配置成本，并输出选择依据。

## 权限模式

| 模式 | 行为 |
|---|---|
| `manual` | 读取自动执行；写入和命令等待确认；高危操作二次确认 |
| `plan` | 隔离的规划 Agent 只使用读取能力，完成后由用户选择执行方式 |
| `auto` | 白名单写入与命令自动执行；未知命令等待确认；高危操作二次确认 |
| `full` | 普通操作自动执行；高危操作显示警告并等待确认 |

启动时指定模式：

```bash
eylu --mode plan
```

TUI 中可通过 `Shift+Tab` 在四种模式间循环。运行期间的切换会在下一轮生效。

### 只读命令判定与权限组合

`plan` 模式只放行能够证明无副作用的命令；`auto` 模式的白名单前缀不会绕过这一判定。

- 命令按命令行逐段解析，只有每一段都能证明无副作用时才归类为只读。
- 参数按命令单独校验。`find` 的 `-delete`、`-exec`、`-execdir`、`-fprint`，`git branch` 的 `-d`、`-D`、`-m`、`-M`、`-c`、`-C`、"`--delete`"、"`--set-upstream-to`"，`git diff`/`git log`/`git show` 的 `--ext-diff`、`--textconv`、`--output`、`--show-signature`，`git grep -O`，以及 `git -c`、`--config-env`、`--exec-path`、`--paginate` 等参数被判定为高危，而不是只读。
- 只读命令的短选项区分大小写，因此 `pwd -P` 是只读，`git grep -O` 会被判定为高危。
- 无法可靠解释的输入回退为 unknown：未闭合引号、变量或命令替换（`$VAR`、`$(...)`）、管道、重定向、反引号，以及在 `read_only_commands` 中自定义、但没有内置参数规则的命令所携带的参数。`plan` 模式拒绝 unknown，`manual`/`auto` 按各自模式要求确认。
- 权限分层为：不可绕过的禁止规则、模式默认策略、工具领域策略（例如 Web 独立权限）、显式审批结果。明确的全局禁止不会被工具级策略放宽；零值或无法识别的权限决策一律按拒绝处理。
- 审计记录保留模式、命中的规则、最终决策和覆盖来源。

行为变化：此前仅按命令前缀无条件放行的参数形式（如 `find . -delete`、`git branch -D`、`git diff --ext-diff`）现在会被判定为高危或 unknown。

命令分类只是应用层策略，不等同于操作系统沙箱：`working_directory` 只决定命令的启动目录，不限制命令自身能访问的路径。

## Skills 与 MCP

Eylu 按以下优先级发现 Agent Skills：

```text
<workspace>/.eylu/skills
<workspace>/.agents/skills
~/.eylu/skills
~/.agents/skills
```

项目级 Skill 需要工作区信任。可以先诊断再启用：

```bash
eylu skills list
eylu skills validate ".agents/skills/code-review"
eylu skills diagnose --output json
```

MCP server 支持 `stdio`、`streamable_http` 和 `sse` 三种传输，配置放在 Eylu TOML 中：

```toml
[mcp_servers.repository]
transport = "stdio"
enabled = true
required = true
command = "repo-mcp"
args = ["serve", "--stdio"]
environment = ["REPO_MCP_TOKEN"]
working_directory = "."
read_only_tools = ["search", "inspect"]
allow_tools = ["search", "inspect", "status"]
deny_tools = ["status"]
startup_timeout_seconds = 60
call_timeout_seconds = 60

[mcp_servers.remote]
transport = "streamable_http"
url = "https://mcp.example.com/rpc"
environment_headers = { "X-API-Key" = "REMOTE_MCP_API_KEY" }
bearer_token_environment = "REMOTE_MCP_BEARER_TOKEN"

[mcp_servers.remote.oauth]
issuer = "https://auth.example.com"
client_id = "eylu"
client_secret_environment = "REMOTE_MCP_CLIENT_SECRET"
scopes = ["mcp:tools", "mcp:resources"]
```

`sse` server 使用相同的 `url`、Header 和 OAuth 字段。静态 Header 可通过 `headers = { Authorization = "Bearer token" }` 直接配置；敏感值也可通过 `environment_headers`、`bearer_token_environment` 或 OAuth 注入。兼容字段 `disabled`、`timeout_seconds`、`read_only_tools` 继续有效。默认启动、调用和 OAuth/交互超时分别为 60、60、30 秒。多个 server 最多并行连接 4 个。Streamable HTTP 握手中的每个 POST 和工具目录分别应用启动期限，HTTP 客户端会保存服务端 Cookie 并发送稳定 User-Agent。工具目录就绪后 server 即进入 connected；日志级别、资源、资源模板与提示词在后台加载，可选目录失败会记录诊断并保留已连接的工具。临时连接错误最多自动重试 3 次，认证、配置和用户取消错误会直接结束；重试耗尽后可执行 `reconnect`。退出时的会话清理采用 2 秒有界等待。

```bash
eylu mcp list
eylu mcp inspect repository --output json
eylu mcp tools repository
eylu mcp tool repository search
eylu mcp resources repository
eylu mcp resource repository "repo://status"
eylu mcp prompts repository
eylu mcp prompt repository review --arguments '{"branch":"main"}'
eylu mcp reconnect repository
eylu mcp enable repository
eylu mcp disable repository
eylu mcp login remote
eylu mcp logout remote
```

TUI 启动后会在 MCP 加载期间于 Banner 下展示 spinner，加载进入成功或失败终态后自动清除该行；输入 `/mcp` 可直接打开 server 列表和详情面板，使用左右方向键或数字键切换详情、工具、资源和提示词。Tools 页只显示可选列表，按 Enter 进入工具详情，按 Esc 返回。TUI 首轮请求会复用启动时建立的 MCP manager。连接错误显示在内容区域并同步保留到聊天历史，HTTP 502 在自动重试耗尽后提供手动重连提示，后台诊断不会穿透到输入区。目录变更通知会原子更新工具注册表、上下文和缓存指纹。OAuth 凭据保存在 `~/.eylu/mcp_credentials.json`，写入采用文件锁、原子替换及平台权限收紧。

MCP 环境变量按名称白名单转发；只读工具仍需在本地配置中显式声明。

## 配置与数据

配置加载优先级：

```text
命令行参数 > EYLU_* 环境变量 > <workspace>/.eylu/config.toml > ~/.eylu/config.toml > 默认值
```

常用路径：

| 内容 | 默认位置 |
|---|---|
| 用户配置 | `~/.eylu/config.toml` |
| 项目配置 | `<workspace>/.eylu/config.toml` |
| 会话与模型缓存 | `~/.eylu/state/` |
| 项目 Skills | `<workspace>/.eylu/skills/`、`<workspace>/.agents/skills/` |

`EYLU_WORKSPACE` 可以覆盖当前工作区，`EYLU_STATE_DIR` 可以修改状态目录。API Key、Provider headers 和其他凭据不会写入会话文件。

### 并行工具调用

Eylu 会让模型在同一轮返回相互独立的工具调用，并根据文件、目录和会话状态依赖进行资源感知调度。只读工具、只读 Bash 命令和不同文件的写入可以并行；同一文件的写入以及交互、会话状态操作会保持有序执行。

```toml
max_parallel_tools = 4
```

默认并发上限为 `4`。设为 `1` 可让工具串行执行；环境变量 `EYLU_MAX_PARALLEL_TOOLS` 可临时覆盖该值。明确声明为只读的 MCP 工具可参与并行调度，其他 MCP 工具采用独占执行。

### 取消、失败与批次终止

- 调度器在每次启动调用前、`OnStart` 返回后、以及取得资源占用之后都会重新检查取消，因此已观察到取消后不会再启动新的调用；取消时排队的等待者会从资源协调器队列中移除。
- 一次请求结束后会保留已经取得的执行结果：已经成功提交的写入不会因为随后的取消而被改写为“未执行”，但请求本身仍会报告取消。
- 工具失败与请求基础设施失败表现不同。普通工具错误交给模型调整；审批通道故障、调度器无法推进等基础设施故障会终止整个预检批次，已经获批的同批调用也不会执行，并按失败返回，而不是伪装成用户中断。
- 多个错误同时出现时，最初的实质性失败保持为主错误，取消原因同时保留，`errors.Is` 对两者都成立。
- 工具超时是协作式取消：执行器依赖工具遵守 context 取消，不会通过启动不可回收的 goroutine 伪造硬性超时。不可信或不可协作的插件需要进程隔离，属于后续增强项。
- 每个调用最多产生一次开始事件和一次终态事件，每个调用最多写入一条审计记录。
- 每次工具批次都会返回宿主拥有的控制状态：`continue`（普通失败，交给模型调整）、`interrupt_request`（用户无理由拒绝）、`cancel_request`（请求 context 被取消）、`abort_request`（审批通道或执行基础设施故障）。调用终态区分为 `succeeded`、`failed`、`rejected`、`cancelled`、`not_executed`、`outcome_unknown`。
- 控制状态由执行器和审批层生成，不读取工具正文、MCP 注解或结果 metadata。外部工具即使伪造 `interrupt_request`、`approval_rejected` 等字段也不能中断或终止宿主请求。
- Web 扇出折叠只负责内容与展示：保留父调用 ID、保留每个子调用的终态、并在折叠结果中保留已成功查询的内容；控制状态不经过聚合层，因此多查询与单查询的控制语义一致。未执行或被拒绝的查询不会被投影成已执行的搜索活动。
- 为兼容旧 UI，结果中仍会输出 `interrupt_request`、`approval_rejected`、`rejection_reason`、`batch_cancelled` 等过渡 metadata；新控制逻辑不读取它们。

### 调用提交与终态闭合

- 模型响应在提交前完成校验：turn 角色与 part 结构、工具调用 ID 是否为空、同一响应内是否重复、参数是否为合法 JSON、调用 ID 在会话历史中的唯一性，以及 Stop 与工具调用是否一致。校验失败的响应不会写入 transcript，用户消息保留，不可信的 driverState 被清除。
- 提交之后的每个工具调用都必须有终态。预算耗尽、工具注册刷新失败、Web 工具解析失败、取消与中断等所有退出路径都会为尚未执行的调用补上 `not_executed` 终态；已经产生的成功结果不会被改写。
- `Stop` 声明完成却仍返回工具调用属于协议矛盾：按协议错误返回，而不是当作正常完成，也不会留下悬空调用。
- 长度截断（`length`）响应保留部分内容，其中的工具调用不会执行，并补上 `not_executed` 终态。
- 已保存的 transcript 保持原样。构建模型请求时，历史上没有结果记录的调用会以 `outcome_unknown` 补全，并且不会自动重放；诊断可通过 `RecoveryNotes` 读取。
- `Send` 与 `Adopt` 不执行工具，它们记录的调用同样会被闭合。

### 代码片段引用与上下文裁剪

代码切片去重（重复读取被替换为稳定引用）只对**正文完整且行范围可靠**的片段生效：

- 上下文窗口裁剪会保留首尾并插入摘要标记。被裁剪的副本会显式标记为不完整（`context_truncated`、`Truncated`），因此不会再被当作 canonical 片段。
- 不完整的大片段不会覆盖更小的完整片段，不会让后续读取退化为引用，也不会重定向已有引用；否则模型会拿到指向并不包含该范围正文的引用。
- 工具自身声明不完整（`lines_complete = false`，例如达到 `max_read_lines`）的读取同样不作为 canonical。
- 文件 hash 变化时不会引用旧版本内容。
- 原始 transcript 不会被修改，被省略的正文不会成为去重依据。token 节省可以让步，内容正确性不能让步。

### 会话事件日志与快照

事件日志（`events.jsonl`）与快照（`snapshot.json`）的进度相互独立，两者都记录会话状态：

- 追加成功即视为持久：日志进度立刻推进。随后快照保存失败只标记“快照落后”，不会回退日志进度。
- 下一次同步不会重复发送已确认的追加事件，会重试快照保存，并只追加真正新增的事件。turn、prompt、skill、runtime、context、driverState 与错误记录使用同一套进度管理。
- 状态类事件（runtime、context、agentTasks、driverState、error）只在负载变化时追加，重复同步不会产生重复事件。
- snapshot 丢失或落后时，仅凭事件日志即可恢复会话；恢复后的 turn 不会重复。
- 追加结果不确定（Write/Sync/Close 报错）时的稳定事件 ID、重复检测与 schema 迁移属于后续阶段（PR-10）。

### 资源冲突键

资源冲突检测使用统一构造的资源键，Bash、读写文件、目录与搜索工具和资源协调器都经由同一入口：

- 路径会做清理、分隔符归一（统一为 `/`）与尾部分隔符处理（卷根保留）。
- Windows 采用保守的大小写归一：可能把两个不同名字串行化，但不会漏掉冲突；漏判冲突才是危险方向。POSIX 不会无条件转小写。
- 只读 Bash 命令的工作区整体读取与同树文件写入一定会被判为冲突。
- 无法确定资源身份（空路径、未知资源类型、未知访问模式）时回退为独占执行。
- 已知边界：硬链接等通过不同路径指向同一文件的别名、UNC 路径与映射盘符、Windows 上的按目录大小写敏感场景不做识别，一律退化为串行执行。

### Web 扇出与执行身份

Web 批量查询会把一次模型调用展开为多次并发执行，三类身份被明确区分：

- 模型调用 ID 由模型提供，只用于与 provider 协议配对；折叠后的工具结果仍使用父调用 ID。
- 执行 ID 由宿主独立分配，不由模型可控 ID 拼接生成，并会避开本轮已占用的模型调用 ID。因此模型同时返回 `a` 与 `a:1` 时，`a` 的扇出不会与 `a:1` 相撞。
- 父调用 ID 显式记录在工具调用与审计记录中，事件、审计与子结果都使用执行 ID 并携带父关系，不解析字符串前缀推断父子关系。
- Web 活动 ID 由宿主执行身份派生，因此启动与完成两次投影结果一致，UI 不会重复显示。
- 结果顺序仍按原始模型调用顺序。

### 代码上下文与后台子代理

`read_file` 支持 1-based 闭区间参数 `start_line`、`end_line`，并返回 `file_hash`、`slice_hash`、`artifact_id` 和续读游标 `next_start_line`。`search_code` 共享会话级增量三元组索引，支持 `offset` 分页和 `context_lines` 上下文；重复或被更大范围覆盖的代码切片在发送给模型前会替换为稳定引用。

模型可通过 `agent` 启动 `search` 或 `general` 子代理。所有任务强制在后台运行并立即返回 `task_id`；兼容输入中的 `run_in_background=false` 会被忽略。`task_output` 只返回即时快照，不会等待或消费完成通知；`task_stop` 会取消当前轮次并清空待处理消息。后台任务进入终态后，空闲的主会话会自动续轮；正在运行的主会话会在下一次模型调用前接收结果。结果以一次性 `<agent_notification>` 注入，多个同时完成的任务会合并交付。

`search` 仅注册 `search_code`、`read_file`、`list_directory` 并返回结构化检索报告。`general` 继承父会话上下文、模型、reasoning effort、权限模式、MCP 和已激活 Skill，使用独立 Conversation 与串行消息队列，并禁用递归 `agent` 调用。多个代理共享工作区、资源协调器和 `max_parallel_agents` 上限；同一路径写入保持有序，子代理的 `write_file` 只创建新文件，现有文件通过 `read_file` 后使用精确 `edit_file`。

TUI 输入 `/agents` 或 `/agents <筛选文本>` 可选择当前 Session 的代理。Enter 打开全屏会话，Enter 发送后续消息，活动任务按 `s` 停止，Esc 返回主会话。权限审批继承父模式并按 FIFO 展示代理来源。Eylu 退出时会取消并等待活动代理，保存终态转录；恢复后的历史代理可查看且为只读。

```toml
max_parallel_agents = 2
code_context_cache_bytes = 67108864
max_read_lines = 2000
code_index_workers = 4

[search_agent]
max_turns = 8
timeout_seconds = 120
# provider = "fast-model" # 省略时继承当前 Provider
# model = "model-id"      # 省略时继承当前模型
```

对应环境变量为 `EYLU_MAX_PARALLEL_AGENTS`、`EYLU_CODE_CONTEXT_CACHE_BYTES`、`EYLU_MAX_READ_LINES` 和 `EYLU_CODE_INDEX_WORKERS`。

## 终端兼容性

- 交互式 TTY 默认启动 Bubble Tea 全屏界面。
- `--no-animation` 保留静态主题并关闭动态效果。
- `--no-tui` 使用纯文本交互。
- `NO_COLOR` 移除 ANSI 颜色。
- `TERM=dumb`、管道和结构化输出自动使用静态路径。

## 项目文档

- [CHANGELOG.md](CHANGELOG.md)：版本变更记录
- [RELEASING.md](RELEASING.md)：版本、签名、CI 和故障恢复流程
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)：第三方组件与适用条款
- [docs/go-terminal-agent-development-plan.md](docs/go-terminal-agent-development-plan.md)：架构与阶段开发记录

## 开发与验证

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

CI 会在 Linux、Windows、macOS 上执行测试、原生构建和 smoke test；发布标签会进一步生成六个平台归档、SHA-256 校验与 Sigstore 签名。

## 许可证

Eylu 由 xnqycs 以 [Apache License 2.0](LICENSE) 发布。第三方组件及其适用条款见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
