# Eylu 终端编程 Agent 开发计划

> 状态：Proposed
>
> 更新时间：2026-07-19
>
> 目标：用 Go 构建一个面向本地代码库的终端编程 Agent，完成“用户请求 → 模型推理 → 工具执行 → 结果回传 → 继续推理”的可审计闭环。

## 1. 目标与边界

### 1.1 产品目标

用户可以在项目目录中启动 Agent，用自然语言完成以下工作：

- 阅读和搜索项目代码。
- 生成修改计划。
- 在获得权限后修改文件。
- 执行测试、构建和诊断命令。
- 发现并按需激活项目级或用户级 Agent Skill，复用领域知识、工作流、脚本和资源。
- 使用 `/new` 清空会话动态上下文并创建新会话，使用 `/context` 在同一视图检查各类上下文占用。
- 在终端内添加、编辑、删除、切换 AI Provider，并从兼容端点选择或手动输入模型 ID。
- 在同一会话中持续追问，并在程序重启后恢复上下文。

Eylu 使用自有命令、配置、会话格式、工具协议和 Agent Loop。外部模型服务通过可插拔 `ModelDriver` 接入，首批驱动可覆盖常见 HTTP 模型协议和兼容网关。Provider、模型、内部 adapter、凭据来源、API 地址、工作目录和权限模式都通过 Eylu 配置、运行时命令或命令行参数控制。

### 1.2 首版边界

首版聚焦本地单工作区和交互式终端。以下能力放入 Phase 9：

- 基于任务、成本和能力的多模型、多 Provider 自动路由。
- 远程执行和分布式任务调度。
- 外部插件市场。
- 完整 MCP 客户端生态。
- 多用户服务端和 Web UI。

### 1.3 关键原则

1. **核心协议稳定**：Eylu 领域模型独立定义请求、事件、工具调用、结果和停止原因，驱动负责外部协议转换。
2. **编排可控**：Agent 循环使用显式状态机、最大迭代次数和总预算，所有异常都有终止路径。
3. **权限前置**：工具执行必须经过统一策略检查，工具自身不绕过策略。
4. **默认安全**：工作区边界、命令超时、输出上限、危险操作确认和日志脱敏作为基础能力尽早落地。
5. **可测试可回放**：ModelDriver、工具和交互层都提供可替换实现，使用固定响应完成离线测试。
6. **渐进交付**：每个 Phase 都有独立的验收标准和可运行产物。
7. **Skill 渐进披露**：会话启动时只注入 Skill 元数据，激活时加载完整指令，引用资源继续按需读取。

### 1.4 Eylu 协议策略

- **自有领域模型**：核心只使用 `ModelRequest`、`ModelEvent`、`ModelResponse`、`ToolCall`、`ToolResult` 和 `StopKind`。
- **驱动式接入**：`ModelDriver` 把 Eylu 请求映射到外部 HTTP/SDK 协议，再把外部响应还原为 Eylu 事件。
- **本地状态为准**：session、审计、恢复和上下文压缩统一依赖 Eylu transcript；远端会话 ID 保存为驱动私有状态。
- **能力协商**：驱动声明文本流、工具调用、并行工具、推理内容、图像输入和远端会话等能力。
- **原始数据封装**：驱动私有 JSON 进入 `DriverState` 和调试附件，Agent Loop、Policy、Tool 与 UI 只读取 Eylu 类型。
- **可扩展注册**：新增模型服务只需注册驱动，核心循环和 session schema 保持稳定。

## 2. 总体架构

### 2.1 分层

```text
cmd/agent
    ↓
internal/app       命令编排、依赖组装、退出码
    ↓
internal/agent     会话循环、状态机、预算、上下文压缩
    ├── driver      模型协议适配、能力声明、流式事件、重试
    ├── tool        工具注册、JSON Schema、执行器
    ├── policy      权限模式、命令分类、确认流程
    ├── session     会话事件、快照、恢复
    ├── provider    Provider 配置、凭据、模型发现、热更新
    ├── context     token 估算、项目地图、摘要
    ├── skill       Skill 发现、校验、优先级、激活、资源解析
    └── ui          终端输入、流式渲染、Markdown、状态栏
```

### 2.2 建议目录

```text
.
├── cmd/agent/main.go
├── internal/
│   ├── app/
│   ├── agent/
│   ├── config/
│   ├── context/
│   ├── logging/
│   ├── policy/
│   ├── provider/
│   │   ├── manager.go
│   │   ├── config.go
│   │   ├── credentials.go
│   │   └── models.go
│   ├── protocol/
│   │   ├── model.go
│   │   ├── event.go
│   │   └── transcript.go
│   ├── driver/
│   │   ├── registry.go
│   │   ├── capabilities.go
│   │   ├── openai_responses/     # 外部协议适配器
│   │   ├── openai_chat/          # 外部协议适配器
│   │   ├── anthropic_messages/   # 外部协议适配器
│   │   └── custom_http/          # 自定义网关适配器
│   ├── session/
│   ├── skill/
│   │   ├── registry.go
│   │   ├── discovery.go
│   │   ├── parser.go
│   │   ├── catalog.go
│   │   └── resolver.go
│   ├── tool/
│   │   ├── registry.go
│   │   ├── read_file.go
│   │   ├── write_file.go
│   │   ├── edit_file.go
│   │   ├── bash.go
│   │   ├── repository_index.go
│   │   ├── search_code.go
│   │   ├── list_directory.go
│   │   ├── activate_skill.go
│   │   └── read_skill_resource.go
│   └── ui/
├── testdata/
│   ├── fixtures/
│   └── transcripts/
├── docs/
├── go.mod
└── go.sum
```

### 2.3 依赖策略

| 能力 | 初始选择 | 约束 |
|---|---|---|
| CLI | `cobra` | 命令和业务逻辑分离，命令层只负责参数解析 |
| HTTP/API | `net/http` + 可选供应商 SDK | 在 `ModelDriver` 后隔离，核心层不导入供应商 SDK 类型 |
| JSON Schema | `encoding/json` + 明确的输入结构 | 工具输入先解码、再校验、再执行 |
| 仓库忽略规则 | `git ls-files --cached --others --exclude-standard -z` | 复用 Git 标准忽略语义，NUL 分隔路径，`search_code` 与 `list_directory` 共享索引 |
| YAML frontmatter | `gopkg.in/yaml.v3` | 只解析 `SKILL.md` 元数据，正文保留原始 Markdown |
| TUI | `charm.land/bubbletea/v2` + `charm.land/bubbles/v2` | Phase 0 用于首次引导，Phase 7 完成交互主界面；网络、模型和工具事件以消息进入单一 Update/View 循环 |
| 终端样式 | `charm.land/lipgloss/v2` | Phase 7 完整引入，核心层不依赖 TUI |
| 凭据存储 | `github.com/zalando/go-keyring` | API Key 写入系统 keyring；支持环境变量引用和当前进程内存回退 |
| Markdown | `glamour` | 渲染失败时回退纯文本 |
| 持久化 | JSONL 事件日志 + JSON 快照 | schema 版本化，写入采用临时文件替换 |

### 2.4 配置优先级

```text
命令行参数 > EYLU_* 环境变量 > .eylu/config.toml > ~/.eylu/config.toml > 默认值
```

建议的配置项：

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `EYLU_CONFIG` | 自动发现 | 显式指定配置文件 |
| `EYLU_PROVIDER` | 读取 `active_provider` | 临时选择当前 Provider |
| `EYLU_API_KEY` | 可选 | Eylu 通用凭据覆盖，日志中永久脱敏 |
| `EYLU_MODEL` | 读取 Provider | 临时覆盖模型 ID |
| `EYLU_BASE_URL` | 读取 Provider | 临时覆盖 Provider API Base URL |
| `EYLU_WORKSPACE` | 当前目录 | 所有文件工具和命令默认受工作区限制 |
| `EYLU_PERMISSION_MODE` | `manual` | `manual`、`plan`、`auto`、`full` |
| `EYLU_MAX_TURNS` | 20 | 一次用户请求允许的模型-工具往返上限 |
| `EYLU_TOOL_TIMEOUT` | 60s | 单个工具执行超时 |
| `EYLU_MAX_OUTPUT_BYTES` | 64 KiB | 工具结果回传模型前的截断上限 |

配置档案示例：

```toml
active_provider = "work"

[providers.work]
adapter = "openai_responses"
base_url = "https://gateway.example.com/v1"
model = "team-coding-model"
context_window = 128000

[providers.work.credential]
type = "keyring"
service = "eylu"
account = "provider:work"

[providers.local]
adapter = "custom_http"
base_url = "http://127.0.0.1:11434"
model = "local-coder"
credential = { type = "none" }
```

Provider 是用户可管理的服务配置，`adapter` 指向内部 `ModelDriver` 实现。API Key 内容不写入 TOML，配置只保存 keyring 引用；`CredentialStore` 首版实现 `keyring`、`env`、`memory` 和 `none`。`context_window` 为可选值，用于 `/context` 计算模型窗口占比。

Skill 采用零配置默认启用模式，Provider、模型和通用运行参数进入 TOML。程序固定扫描项目级和用户级约定目录；扫描数量、入口大小、资源大小和递归深度使用代码内安全常量，并通过 `eylu skills diagnose` 展示实际值。

### 2.5 Provider 运行时与首次引导

`ProviderManager` 维护已持久化配置和当前活动 Provider 的不可变运行时快照。添加或编辑流程先解析并校验候选配置，再原子写入用户级配置文件，最后原子替换活动快照；正在执行的请求持有原快照，后续请求直接读取新快照。切换 Provider 或修改活动 Provider 的 adapter、API 地址、凭据、模型后，清空供应商私有 `DriverState`，保留 Eylu transcript，并按新上下文窗口重新检查预算。

交互命令统一为：

```text
/providers                 列出并打开 Provider 管理视图
/provider add              添加 Provider
/provider edit <name>      修改 Provider
/provider delete <name>    删除 Provider
/provider use <name>       切换活动 Provider
/model                     刷新并选择当前 Provider 的模型
```

CLI 同时提供 `eylu providers list|add|edit|delete|use|models`，便于脚本和故障恢复。删除活动 Provider 时先选择替代项；删除最后一个 Provider 后进入首次配置引导。

TTY 启动时发现有效 Provider 数量为 0，直接展示分步引导：Provider 名称、adapter、API Base URL、遮罩 API Key、模型选择、可选上下文窗口。OpenAI-compatible adapter 把包含 `/v1` 的 API Base URL 与 `/models` 拼接，默认使用 `Authorization: Bearer <key>` 请求 `GET /v1/models`，解析 `data[].id`，去重并稳定排序后提供搜索选择；发送前在 Key 输入框旁明确展示目标 host。列表请求失败、返回空列表或服务端缺少该端点时，界面保留诊断并直接进入模型 ID 手输框；模型选择页始终提供“手动输入模型 ID”。

模型列表接口只保证基础模型信息，`context_window` 由 adapter 扩展元数据、用户输入或后续模型能力注册表提供。上限来源缺失时 `/context` 仍显示已用 token 和分类占比，并把窗口上限标记为 `unknown`。非 TTY 启动且 Provider 为空时输出结构化错误和 `eylu providers add` 用法。

API Key 输入组件关闭回显。keyring 可用时保存到系统凭据库；keyring 服务缺失时提供环境变量引用或仅当前进程有效的 `memory` 存储选择。Provider 更新失败保持原配置和原运行时快照，并显示字段级错误。配置写入使用进程锁、临时文件、刷新和原子替换。

模型列表请求设置独立的 10 秒超时、2 MiB 响应上限和 5000 个模型条目上限，支持 context 取消。Provider 重命名时同步迁移 keyring account；删除时清理对应凭据。任何配置或凭据步骤失败都会回滚候选变更，活动 generation 保持原值。

### 2.6 依赖方向

```text
cmd/app ──→ agent ──→ protocol ←── driver implementations
                ├──→ tool
                ├──→ policy
                ├──→ provider
                ├──→ context
                ├──→ skill
                └──→ session
```

- `internal/protocol` 只依赖 Go 标准库，定义 Eylu v1 类型。
- `internal/agent` 只接收 `ModelDriver` 接口，驱动实现由 `internal/app` 注入。
- `internal/provider` 管理 Provider 配置、凭据引用、模型目录和 driver 快照；Provider 名称属于用户配置，Driver 名称属于内部 adapter registry。
- `internal/skill` 只负责本地 Skill 的发现、解析和资源定位；模型调用与脚本执行继续由既有分层负责。
- `internal/driver/*` 可以依赖外部 SDK，并负责协议字段、鉴权头、SSE 事件和错误码映射。
- `internal/session` 保存 Eylu transcript 与 opaque driver state，供应商原始响应进入可选调试附件。
- CI 增加 import 规则检查，阻止 `internal/agent`、`internal/protocol`、`internal/tool`、`internal/policy` 引入供应商 SDK。

## 3. 核心数据契约

### 3.1 Eylu transcript 与工具调用

核心层使用 Eylu 自有结构，session 直接序列化这些类型：

```go
type Role string

const (
    RoleUser      Role = "user"
    RoleAgent     Role = "agent"
    RoleTool      Role = "tool"
)

type PartKind string

const (
    PartText       PartKind = "text"
    PartReasoning  PartKind = "reasoning"
    PartToolCall   PartKind = "tool_call"
    PartToolResult PartKind = "tool_result"
)

type Turn struct {
    ID    string
    Role  Role
    Parts []Part
}

type Part struct {
    Kind       PartKind
    Text       string
    ToolCall   *ToolCall
    ToolResult *ToolResult
}

type ToolCall struct {
    ID    string
    Name  string
    Input json.RawMessage
}

type ToolResult struct {
    CallID  string
    Output  string
    IsError bool
}
```

Eylu 为每个 turn 和 tool call 生成稳定 ID。驱动负责维护外部调用 ID、远端响应 ID 和其他关联信息，并将其写入 opaque `DriverState`。Agent Loop 只依赖 Eylu ID 和类型。

### 3.2 ModelDriver 接口

```go
type StopKind string

const (
    StopCompleted StopKind = "completed"
    StopNeedsTools StopKind = "needs_tools"
    StopLength    StopKind = "length"
    StopFailed    StopKind = "failed"
)

type DriverCapabilities struct {
    Streaming         bool
    ToolCalls         bool
    ParallelToolCalls bool
    ReasoningParts    bool
    RemoteState       bool
}

type ModelRequest struct {
    Model             string
    Instructions      string
    Transcript        []Turn
    Tools             []ToolDefinition
    MaxOutputTokens   int
    Temperature       *float64
    ParallelToolCalls *bool
    DriverState       []byte
}

type ModelResponse struct {
    Turn        Turn
    Stop        StopKind
    Usage       Usage
    DriverState []byte
    TraceID     string
}

type ModelDriver interface {
    Name() string
    Capabilities() DriverCapabilities
    Generate(ctx context.Context, req ModelRequest) (ModelResponse, error)
    Stream(ctx context.Context, req ModelRequest) (<-chan ModelEvent, error)
}
```

`ModelEvent` 统一表示响应开始、文本增量、推理增量、工具输入增量、工具调用完成、usage、响应完成和错误。驱动将外部事件聚合为完整 Eylu turn，再交给 Agent Loop。连接错误、限流、服务端错误、上下文超限和认证错误统一映射到 Eylu error taxonomy。未知外部事件保存到调试附件并生成诊断事件。

驱动 contract test 必须验证双向映射：Eylu 请求到外部请求、外部响应到 Eylu turn、Eylu 工具结果到外部工具结果。每个驱动可以使用 SDK 或 `net/http`，核心包只依赖 `ModelDriver`。

### 3.3 Provider 与模型发现契约

```go
type ProviderConfig struct {
    Name          string
    Adapter       string
    BaseURL       string
    Model         string
    ContextWindow *int
    Credential    CredentialRef
}

type ModelInfo struct {
    ID            string
    OwnedBy       string
    ContextWindow *int
}

type ProviderSnapshot struct {
    Config     ProviderConfig
    Driver     ModelDriver
    Generation uint64
}

type ModelLister interface {
    ListModels(ctx context.Context, provider ProviderSnapshot) ([]ModelInfo, error)
}

type ProviderManager interface {
    List() []ProviderConfig
    Add(ctx context.Context, candidate ProviderConfig, secret SecretInput) error
    Update(ctx context.Context, name string, candidate ProviderConfig, secret *SecretInput) error
    Delete(ctx context.Context, name string) error
    Use(ctx context.Context, name string) error
    Current() (ProviderSnapshot, bool)
}
```

`ProviderManager` 对读取暴露不可变快照，对写入串行化。模型请求在开始时捕获一次快照，request 生命周期内保持相同 generation。OpenAI-compatible `ModelLister` 请求 `{BaseURL}/models`，对应常见的 `/v1/models` 端点；解析 `object=list` 与 `data[].id`，同时容忍兼容网关省略 `object` 或额外返回字段。手工输入的模型 ID 直接保存为 opaque string，由真实模型请求验证可用性。

### 3.4 Tool 接口

```go
type RiskLevel string

const (
    RiskRead   RiskLevel = "readonly"
    RiskWrite  RiskLevel = "write"
    RiskExec   RiskLevel = "exec"
    RiskDanger RiskLevel = "dangerous"
)

type ToolDefinition struct {
    Name        string
    Description string
    InputSchema json.RawMessage
    Risk        RiskLevel
}

type Tool interface {
    Definition() ToolDefinition
    Execute(ctx context.Context, input json.RawMessage) ToolResult
}
```

`InputSchema` 使用 JSON Schema 描述 Eylu 工具输入。各驱动自行映射外部工具定义格式，并在外部协议支持时启用严格 schema。`ToolResult` 必须包含结构化的成功/失败状态、摘要、详细输出、耗时和可选退出码。未注册工具、JSON 输入错误、策略拒绝和执行失败都转换为 Eylu 工具结果，并保留本地日志中的完整诊断。

### 3.5 Skill 契约与加载模型

Eylu 兼容 [Agent Skills Specification](https://agentskills.io/specification)。一个 Skill 是包含 `SKILL.md` 的目录，可选包含 `scripts/`、`references/`、`assets/` 等资源：

```text
code-review/
├── SKILL.md
├── scripts/
├── references/
└── assets/
```

`SKILL.md` 使用 YAML frontmatter 和 Markdown 正文。运行时至少维护以下领域对象：

```go
type SkillSource string

const (
    SkillSourceProject SkillSource = "project"
    SkillSourceUser    SkillSource = "user"
    SkillSourceBuiltin SkillSource = "builtin"
)

type SkillMetadata struct {
    Name          string
    Description   string
    License       string
    Compatibility string
    Metadata      map[string]string
    AllowedTools  []string
}

type Skill struct {
    Metadata  SkillMetadata
    Source    SkillSource
    Root      string
    Entry     string
    Digest    string
}

type SkillResource struct {
    Path string
    Kind string
    Size int64
}

type ActivatedSkill struct {
    Skill     Skill
    Body      string
    Resources []SkillResource
}

type SkillRegistry interface {
    Catalog() []Skill
    Get(name string) (Skill, bool)
    Activate(ctx context.Context, name string) (ActivatedSkill, error)
}
```

发现目录按以下优先级处理，同名 Skill 由高优先级覆盖，并记录 shadow 诊断：

```text
<workspace>/.eylu/skills
<workspace>/.agents/skills
~/.eylu/skills
~/.agents/skills
内置 Skill
```

项目级范围整体高于用户级范围。扫描只接受子目录中名称精确为 `SKILL.md` 的入口，设置目录深度、目录数量、文件大小和符号链接边界；忽略 `.git`、`node_modules`、`vendor` 等目录。Skill 功能随程序启动直接启用；项目级 Skill 在工作区受信任后进入模型可见目录。

首版使用固定安全上限：扫描深度 6、遍历目录 2000、有效 Skill 200、单个 `SKILL.md` 512 KiB、单个文本资源 1 MiB、单个 Skill 资源清单 2000 项。达到上限时保留已稳定排序的结果并输出诊断。

Skill 采用三级渐进披露：

1. 会话启动：解析 frontmatter，只把有效 Skill 的 `name`、`description` 和来源写入紧凑 catalog。
2. Skill 激活：模型调用只读的 `activate_skill(name)`，或用户执行 `/skill <name>`；Agent Loop 将去除 frontmatter 的正文加入 protected context block，工具结果返回名称、Skill 根目录、digest 和资源清单。
3. 资源读取：模型通过 `read_skill_resource(name, path)` 按需读取资源；该工具只允许访问已解析 Skill 根目录内的文件，并拒绝路径穿越和符号链接逃逸。

`activate_skill` 的 `name` schema 使用当前 catalog 构造 enum。空 catalog 时省略两个 Skill 工具和 Skill 提示块。Agent Loop 记录本会话已激活的 Skill 并去重；内容压缩时将激活正文标记为 protected。Skill 正文通过结构化边界加入 Eylu instructions，外部驱动接收普通文本，Eylu 核心保留 Skill 类型和审计信息。

`allowed-tools` 仍属于 Agent Skills 的实验字段。Eylu 保存并展示该声明，用于提示生成和调用审计；本地 `Policy` 始终拥有最终裁决权，所有确认流程保持生效。Skill 中的脚本统一通过现有 `bash` 工具执行，继续接受权限模式、超时、工作区、网络和审计约束。

### 3.6 Agent Loop 状态机

```text
Idle
  → BuildRequest
  → StreamingResponse
  → AppendAgentTurn
  → InspectStopReason
      ├─ completed      → RenderFinal → Idle
      ├─ needs_tools    → CheckPolicy
      │                    ├─ deny     → AppendToolError → BuildRequest
      │                    ├─ confirm  → WaitApproval → ExecuteTools
      │                    └─ allow    → ExecuteTools
      │                                  → AppendToolResults → BuildRequest
      ├─ length         → ContinueOrCompact
      └─ failed         → RenderError → Idle
```

循环必须具备以下保护：

- 每次用户请求最多 `EYLU_MAX_TURNS` 次往返。
- 工具调用必须使用唯一 ID，结果按 ID 对齐。
- 单次工具结果和总上下文都限制字节数或 token 数。
- 上下文取消、用户 Ctrl-C、ModelDriver 错误都能释放 goroutine 和子进程。
- 达到预算时输出可理解的终止原因，允许用户继续或重新开始。

### 3.7 ContextLedger 与 `/context`

PromptBuilder 在拼装每个模型请求时同步写入 `ContextLedger`，使 UI 展示的数据与实际发送内容来自同一份 block 清单：

```go
type ContextCategory string

const (
    ContextSystemPrompt      ContextCategory = "system_prompt"
    ContextSkillCatalog      ContextCategory = "skill_catalog"
    ContextSkillInstructions ContextCategory = "skill_instructions"
    ContextMCPInstructions   ContextCategory = "mcp_instructions"
    ContextMCPTools          ContextCategory = "mcp_tools"
    ContextMCPResources      ContextCategory = "mcp_resources"
    ContextMCPToolResults    ContextCategory = "mcp_tool_results"
    ContextToolSchemas       ContextCategory = "tool_schemas"
    ContextUserMessages      ContextCategory = "user_messages"
    ContextAgentMessages     ContextCategory = "agent_messages"
    ContextToolResults       ContextCategory = "tool_results"
    ContextProject           ContextCategory = "project_context"
    ContextSummary           ContextCategory = "summary"
    ContextDriverState       ContextCategory = "driver_state"
    ContextReservedOutput    ContextCategory = "reserved_output"
)

type ContextUsage struct {
    Category  ContextCategory
    Blocks    int
    Bytes     int64
    Tokens    int
    Estimated bool
}

type ContextReport struct {
    Items             []ContextUsage
    InputTokens       int
    ReservedOutput    int
    ContextWindow     *int
    ContextWindowFrom string
    LastProviderUsage *Usage
}
```

`/context` 在一个表中展示所有分类，当前为空的分类显示 0，并为 Skill 和 MCP 提供可展开子项。每行包含 block 数、字节数、token、占当前输入比例和估算标记；表头展示 `input + reserved output`、上下文窗口、总占比、活动 Provider、模型和上限来源。Provider 返回的最近一次真实 usage 单独展示，用来校准本地估算。`ContextWindowFrom` 使用 `provider_config`、`driver_metadata`、`registry` 或 `unknown`；上限未知时展示分类占比与 token 总数，窗口占比显示 `unknown`。

`/new` 先提交当前 session 事件和快照，再创建新的 session ID，清空 transcript、summary、DriverState、已激活 Skill、MCP 会话资源和 token 计数。新 session 重新构建 system prompt、内置工具 schema、Skill catalog 和已配置 MCP 定义作为基线上下文。工作区、活动 Provider、TUI 偏好与当前权限模式作为运行时选择进入新 session；历史 session 保留，可通过恢复命令重新打开。

## 4. 分阶段交付

每个 Phase 包含目标、范围、实现任务、测试和验收。Phase 之间保持可运行状态。

### Phase 0：项目初始化与最小 API 调用

**目标**：建立可复现的 Go 项目骨架，完成一次真实或 mock 的模型请求。

**本期范围**：

- `go mod init`、Go 版本和依赖锁定。
- `cobra` 根命令和 `eylu chat [prompt]`。
- Provider 配置加载、参数校验、统一错误和退出码。
- `DriverRegistry`、`ModelDriver` 和一个可配置真实 HTTP 驱动。
- `ProviderManager`、`CredentialStore`、首次启动引导、凭据脱敏日志和请求超时。

**本期暂缓**：对话历史、工具、流式 UI、会话恢复。

**实现任务**：

- 定义 Eylu protocol v1：turn、part、model event、tool call、tool result、stop kind 和 error code。
- 为 protocol 类型建立 JSON fixture 和向后兼容测试，session schema 引用明确版本。
- 定义 `config.Config` 与 `ProviderConfig`，验证活动 Provider、adapter、凭据引用、API Base URL、模型、上下文窗口、超时和工作目录。
- 为 ModelDriver 注入 `http.Client` 或 SDK client，便于测试替换。
- 实现 `eylu providers list|add|edit|delete|use|models` 与配置原子写入；API Key 使用遮罩输入并存入 `CredentialStore`。
- 实现 OpenAI-compatible `ModelLister`：请求 `{BaseURL}/models`，解析 `data[].id`，支持搜索选择、刷新和手工模型 ID。
- Provider 为空且 stdin 为 TTY 时进入配置引导；非 TTY 模式输出结构化配置错误和恢复命令。
- 将 `go run . chat "你好"` 的成功响应打印到 stdout，诊断信息打印到 stderr。
- 增加 `--provider`、`--model`、`--base-url`、`--timeout` 参数。
- 准备本地 fake server，测试模型列表、请求头、请求体、手工模型 ID 和错误映射。

**验收标准**：

```text
# POSIX shell
EYLU_API_KEY=... go run . chat "你好" --provider work

# PowerShell
$env:EYLU_API_KEY="..."; go run . chat "你好" --provider work
```

首次启动能在引导中完成 API 地址、Key 和模型配置；模型列表来自 `/v1/models` 兼容端点，并始终支持手工模型 ID；能从当前 Provider 收到文本；缺少凭据、超时、HTTP 4xx/5xx 时有明确错误和非零退出码；日志和 TOML 中没有凭据内容。

**退出条件**：`go test ./...`、`go vet ./...`、`gofmt -l .` 通过，真实驱动和 fake server 各完成一次验证；`internal/agent`、`internal/tool`、`internal/policy`、`internal/session` 不导入供应商 SDK 包。

### Phase 1：最小可用多轮对话

**目标**：完成无工具的多轮对话和流式输出。

**实现任务**：

- 维护 Eylu `[]Turn` transcript；驱动状态单独保存在 `DriverState`。
- 实现 `StreamEvent`：文本增量、工具参数增量、响应开始/结束、错误、usage。
- 将增量文本直接写入 UI，结束后合并为一条 agent turn。
- 支持 stdin 交互循环和单次 prompt 两种入口。
- 实现 `/help`、`/new`、`/context`、`/providers`、`/provider add|edit|delete|use`、`/model`、`/quit`。
- `/new` 关闭当前内存 session 边界并创建新 session ID，清空会话动态上下文并重建基线上下文；保留工作区、活动 Provider、TUI 偏好和当前权限模式。Phase 8 接入持久化后先刷新旧 session 快照。
- `/context` 使用 `ContextLedger` 同表展示 system prompt、Skill、MCP、工具 schema、user/agent message、工具结果、项目上下文、摘要和输出预留。
- Provider 添加、修改、删除和切换通过 `ProviderManager` 即时生效；活动 Provider 变化时清理旧 `DriverState` 并重新计算上下文预算。
- Ctrl-C 取消当前请求，第二次 Ctrl-C 退出进程。

**测试**：

- 多轮历史顺序测试。
- `/new` 的 session 边界、上下文清空、旧会话保留和运行时选择继承测试。
- `/context` 分类求和、空分类、未知窗口、估算标记和最近 provider usage 测试。
- Provider 热更新 generation、在途请求快照、下一请求即时生效、删除活动项和最后一项删除测试。
- 流式片段合并测试，覆盖空片段和错误事件。
- fake ModelDriver 断线、取消和空响应测试。

**验收标准**：连续三轮对话能引用前文；执行 `/context` 能看到同表分类占用；执行 `/new` 后获得新 session ID，旧消息与会话资源清空，`/context` 只显示新会话基线；Provider 修改后的下一次请求直接使用新配置；至少两个参考驱动使用同一 Agent Loop 呈现增量输出；取消请求后可继续下一轮。

### Phase 2：工具调用闭环

**目标**：实现 Agent 的核心“请求工具 → 本地执行 → 回传结果”循环。

**实现任务**：

- 建立 `ToolRegistry`，按名称注册和查询工具定义。
- 实现 `read_file`、`write_file`、`bash`。
- 每个工具完成输入 schema 校验、超时、输出截断和结构化错误。
- 解析 agent turn 中的所有 `PartToolCall`，支持同一响应中的多个工具调用。
- 用 Eylu `ToolCall.ID` 关联结果，驱动负责外部调用 ID 映射。
- 先追加完整 agent turn，再追加包含 `PartToolResult` 的 tool turn。
- 循环直到 `StopCompleted`，或命中迭代、超时、预算上限。
- 在工具执行前预留 `Policy.Check` 调用点；Phase 4 完善策略，Phase 2 先实现默认确认回调。

**工具约束**：

- `read_file`：仅允许工作区内路径，限制最大读取字节数，拒绝目录和不可读文件。
- `write_file`：父目录创建需显式允许，使用临时文件加原子替换，报告写入字节数。
- `bash`：使用 `exec.CommandContext`，通过平台 shell adapter 执行，继承最小环境变量，记录 stdout/stderr/退出码。
- 工具描述需要写清用途、适用时机、参数语义、限制和返回内容，复杂输入可附带 schema 校验的示例。
- 所有工具结果回传前截断，并保留 `truncated=true` 元数据。

**测试**：

- 工具 registry、schema、未知工具和坏输入测试。
- fake ModelDriver 驱动的完整 transcript 测试：读取 Go 文件后请求 `bash` 执行 `go build`。
- 多工具并列调用和工具失败后继续推理测试。
- 超时、取消、输出超限、循环上限测试。

**验收标准**：用户可以让 Eylu 读取 Go 文件、分析代码并调用 `bash` 运行构建；同一工具闭环可经不同驱动完成；工具失败会形成可解释的 Eylu 错误结果。

**退出条件**：闭环测试在无网络环境下通过，真实 API 完成一次人工验收；日志能关联 request、tool-use ID 和执行结果。

### Phase 3：精确编辑与项目探索

**目标**：让模型进行局部、可审查、可回滚的代码修改。

**实现任务**：

- `edit_file`：接收 `path`、`old_string`、`new_string`、`expected_replacements`；默认要求匹配次数恰好为 1。
- 写入前生成 unified diff，工具结果返回摘要和 diff 统计。
- 保留文件权限和换行风格，使用原子替换。
- 建立共享 `RepositoryIndex`。先通过 `git rev-parse --show-toplevel` 解析仓库根，再执行 `git ls-files --cached --others --exclude-standard --full-name -z` 获取已跟踪文件和符合 Git 标准忽略规则的未跟踪文件，覆盖根目录与嵌套 `.gitignore`、`.git/info/exclude` 和 Git 全局 excludes。
- 将 Git 输出解析为仓库根绝对路径，筛回 Eylu 工作区范围，并按当前文件系统状态剔除已删除的索引项；工作区位于仓库子目录时保持相同语义。
- `search_code`：在 `RepositoryIndex` 可见文件集合中执行关键词或正则搜索，再应用用户传入的文件 glob、结果数和单文件大小限制；二进制文件跳过。
- `list_directory`：从同一可见文件集合构建稳定目录树，支持根目录、深度、隐藏文件开关和条目上限；Git 元数据目录由仓库边界处理。
- 普通目录或 Git 命令缺失时按文件系统遍历，并继续应用深度、条目数、文件大小、隐藏文件和符号链接安全边界。
- 将工作区根目录和结果上限加入配置；仓库忽略行为以 Git 结果为准。

**边界行为**：

- `old_string` 匹配 0 次或多次时返回失败并要求模型重新读取。
- 路径解析后使用 `filepath.Rel` 校验仍在工作区内，并明确符号链接策略。
- Git 文件索引使用 NUL 分隔解析，支持空格、换行和非 ASCII 路径；命令失败时返回诊断并使用普通目录回退路径。
- 搜索结果和目录树按稳定排序，确保 transcript 可复现。

**测试与验收**：

- 唯一匹配、重复匹配、空替换、编码错误、换行差异测试。
- 路径穿越、符号链接、二进制和大文件测试。
- 根目录与嵌套 `.gitignore`、否定规则、`.git/info/exclude`、全局 excludes、已跟踪但后来被忽略的文件、已删除索引项、仓库子目录工作区、特殊字符路径和 Git 缺失回退测试。
- 验收：Agent 能定位一个函数并只修改该函数，diff 清晰，其他内容保持不变。

### Phase 4：安全与四种权限模式

**目标**：在真实项目操作前建立统一、可审计的权限边界。

#### 4.1 权限模型

```go
type PermissionMode int

const (
    ModeManual PermissionMode = iota
    ModePlan
    ModeAuto
    ModeFull
)

type Decision int

const (
    DecisionAllow Decision = iota
    DecisionConfirm
    DecisionDeny
)
```

| 模式 | 只读工具 | 写文件 | 命令执行 | 高危操作 |
|---|---|---|---|---|
| `manual` | 放行 | 确认 | 确认 | 二次确认 |
| `plan` | 放行 | 拒绝并返回计划提示 | 只放行只读命令 | 拒绝 |
| `auto` | 放行 | 放行 | 白名单放行，其他确认 | 二次确认 |
| `full` | 放行 | 放行 | 放行 | 醒目警告并确认 |

策略检查统一发生在 `ToolExecutor` 内，模型提示词只负责告知可用权限，最终裁决由本地策略完成。

#### 4.2 策略与安全任务

- 实现 `Policy.Check(mode, tool, input, workspace)`，返回 `allow/confirm/deny` 和人类可读原因。
- `activate_skill` 与 `read_skill_resource` 标记为只读工具；资源边界由 Skill resolver 校验，工具执行仍统一进入 `ToolExecutor`。
- 已激活 Skill 根目录加入只读资源边界。Skill 脚本通过绝对入口路径交给 `bash`，其风险级别保持 `exec`，继续遵循当前权限模式。
- 项目级 Skill 的工作区信任确认独立于单次工具确认，信任记录展示规范化路径并支持撤销。
- Manual 模式确认显示工具名、关键参数、目标路径、命令和风险级别。
- Plan 模式禁止写操作；命令分类器允许 `ls`、`git status`、`git diff` 等只读命令。
- Auto 模式仅对配置白名单命令自动放行，未知命令进入确认流程。
- Full 模式对 `rm -rf`、`git reset --hard`、`git push --force`、磁盘格式化、系统目录写入等操作始终显示警告并请求确认。
- 命令白名单、黑名单、工作区目录和环境变量继承规则可配置。
- bash 默认超时；子进程随 context 取消；输出、并发和资源上限可配置。
- 所有工具调用记录模式、决策、确认者、输入摘要、耗时、退出码和结果摘要。
- `/mode manual|plan|auto|full` 支持运行时切换；模式变更写入 session 事件。
- Shift+Tab 循环切换作为 Phase 7 TUI 功能，命令方式先交付。

#### 4.3 Plan 模式收尾

系统提示词明确：当前阶段可读取、搜索和列目录，模型最终输出修改计划、文件清单、风险和验证命令。用户确认后执行 `/mode auto`，保留原会话并继续执行计划。

**测试与验收**：

1. Plan 模式重构请求只产生读取/搜索调用和文字计划。
2. Manual 模式写文件和命令执行均出现确认提示。
3. Auto 模式编辑直接执行，未知命令触发确认。
4. Full 模式执行普通命令，高危操作出现警告和二次确认。
5. 策略单元测试覆盖模式 × 风险级别 × 命令分类矩阵。

### Phase 5：Agent Skills（默认启用）

**目标**：默认启用兼容 Agent Skills 规范的本地 Skill，完成发现、披露、激活、资源读取和审计闭环。

**实现任务**：

- 启动时扫描 `<workspace>/.eylu/skills`、`<workspace>/.agents/skills`、`~/.eylu/skills`、`~/.agents/skills` 和内置 Skill；固定优先级处理同名覆盖并输出诊断。
- 解析 `SKILL.md` YAML frontmatter，支持规范字段 `name`、`description`、`license`、`compatibility`、`metadata` 和实验性 `allowed-tools`。
- 校验名称字符集、长度、目录名一致性、description 长度和文件大小；YAML 解析失败、必填字段缺失或入口超限时跳过该 Skill 并记录位置与原因。
- 工作区首次加载项目级 Skill 时接入项目信任确认；信任结果保存到用户级 Eylu 状态目录并绑定规范化工作区路径。
- 生成只含名称、描述和来源的稳定 catalog；PromptBuilder 只在 catalog 有内容时加入 Skill 使用说明。
- 实现 `activate_skill` 只读工具，按名称建立 protected context block，并返回根目录、SHA-256 digest 和受限资源清单；同一 digest 在单会话内只注入一次。
- 实现 `read_skill_resource` 只读工具，限定已发现的 Skill 根目录、最大读取大小和支持的文本类型，校验路径穿越、符号链接与竞态替换。
- 支持模型自主激活和用户显式 `/skill <name>` 激活；增加 `/skills` 查看有效、shadowed 和 invalid Skill。
- 增加 `eylu skills list|show|validate|diagnose`，其中 `validate` 提供适合 CI 的非零退出码，`diagnose` 显示扫描目录、内置限制、冲突和信任状态。
- 激活事件记录名称、来源、入口、digest、触发方和时间；Skill 正文与资源内容进入日志前只保留摘要和大小。
- `allowed-tools` 作为实验性声明进入提示和审计。脚本执行、网络访问、文件修改和命令确认继续经过 `ToolExecutor` 与 `Policy.Check`。

**测试**：

- frontmatter parser 表驱动测试，覆盖规范字段、CRLF、BOM、空正文、坏 YAML、超长字段和目录名不一致。
- 固定目录发现、同名优先级、稳定排序、目录上限、符号链接和项目未信任状态测试。
- 渐进披露测试：初始请求只含 catalog，激活后才含正文，资源只在读取时进入 transcript。
- 显式激活、模型激活、重复激活、未知名称、内容变更和会话 digest 更新测试。
- `allowed-tools` × 四种权限模式矩阵测试，验证不同声明值下本地 `Policy` 始终保持最终权限上限。
- fake ModelDriver 完整流程：选择 Skill、读取 reference、执行脚本、回传结果并完成回复。

**验收标准**：把一个符合规范的 Skill 放入任一约定目录，重新启动 Eylu 后可直接被发现和激活；初始上下文只增加 catalog；项目级 Skill 经过工作区信任；资源访问保持在 Skill 根目录内；脚本执行保留现有确认、超时和审计行为。

**退出条件**：`eylu skills validate <dir>` 可用于 CI；跨平台路径测试通过；固定响应 transcript 能证明发现、激活、资源读取、策略裁决和去重行为；无 Skill 的工作区保持原有请求内容。

### Phase 6：上下文管理

**目标**：在长会话中维持有效上下文、可预测成本和完整工具消息配对。

**实现任务**：

- 提供 `TokenEstimator` 接口；首版用可配置近似估算，后续接入官方 tokenizer 或供应商计数接口。
- PromptBuilder 生成 `ContextBlock` 时同步登记类别、来源、字节数和 token 估算，保证 `/context` 与实际请求使用同一份输入清单。
- 分别统计 system prompt、Skill catalog、Skill 正文、MCP instructions、MCP tool schema、MCP resources、MCP tool result、内置工具 schema、user message、agent message、内置工具 result、项目上下文、摘要、driver state 和输出预留。
- `/context` 将全部类别放在一张表中，顶层展示总输入、输出预留、上下文窗口和占比；Skill 与 MCP 行支持展开到单个 Skill、MCP server、tool 或 resource。
- 为 Skill catalog、已激活正文和 Skill 资源分别统计预算；catalog 超过上限时按稳定顺序分页披露。
- 上下文超限时按“系统提示 + 项目地图 + 最近完整轮次 + 摘要”重建请求。
- 摘要前保留用户目标、已完成修改、未完成任务、失败尝试和验证结果。
- 禁止截断 `PartToolCall` 与对应 `PartToolResult`；reasoning part 按驱动能力和会话策略保存。
- 已激活 Skill 正文作为 protected context block 保留，并按名称和 digest 去重；摘要记录 Skill 名称、来源和当前 digest。
- 支持驱动使用远端状态减少重复传输，session 仍保存 Eylu 完整 transcript。
- 生成项目地图：稳定文件树、语言统计、入口文件、配置文件和最近修改文件。
- 工具输出优先保存到本地 session，模型上下文只保留摘要和受限片段。

`/context` 文本模式示例：

```text
Context  21,840 input + 8,192 reserved / 128,000  (23.5%)
Provider work · model team-coding-model · limit provider_config

Category             Blocks    Tokens   Input
System prompt             1     2,180    10.0%
Skills                   3     3,420    15.7%
MCP                      8     4,110    18.8%
Tool schemas             7     2,760    12.6%
User messages            6     2,940    13.5%
Agent messages           5     3,180    14.6%
Tool results             4     1,650     7.6%
Project context          2     1,120     5.1%
Summary                  1       480     2.2%
Driver state             0         0     0.0%
```

交互 TUI 使用固定宽度进度条展示占比并允许展开；`--output json|jsonl` 输出稳定字段，便于自动监控。所有数字标记 `exact` 或 `estimated`，最近一次 Provider usage 在表尾单列显示。

**验收标准**：连续修改 20 个文件的会话能触发压缩并继续；`/context` 能在同表区分 system prompt、Skill、MCP、user message 等分类且总和一致；压缩后模型仍能引用当前目标和最近修改；同一压缩结果可交给不同驱动；预算和压缩事件出现在日志中。

### Phase 7：终端体验

**目标**：交付稳定的 Bubble Tea TUI，让用户清楚区分输入、模型文本、工具调用、确认、错误和所有进行中状态。

**实现任务**：

- 使用 Bubble Tea v2 的 `Model → Update → View` 单写入循环，模型流、工具输出、Provider 请求、计时器和按键都转换为 `tea.Msg`。
- `lipgloss` 定义 user、agent、tool、warning、error、status、loading 七类样式。
- `glamour` 渲染 Markdown；终端不支持颜色时自动降级。
- 工具调用显示名称、参数摘要、运行中状态、耗时和结果摘要；详细输出支持折叠或分页。
- `/skills` 显示来源、有效状态和 shadow 关系；`/skill <name>` 提供名称补全并标记当前会话已激活项。
- `/providers` 使用列表与表单视图完成添加、修改、删除和切换；API Key 使用 password input，模型列表支持 loading、搜索、刷新、选择和手工输入。
- `/context` 使用统一表格和稳定宽度进度条展示分类占用，支持展开 Skill 与 MCP 明细。
- 定义 `idle`、`connecting`、`fetching_models`、`waiting_first_token`、`streaming`、`executing_tool`、`awaiting_approval`、`retry_backoff`、`cancelling`、`completed`、`failed` UI 状态。
- loading 区域使用 Bubbles `spinner.Model.Tick()` 驱动固定宽度动画，同时显示当前操作、活动 Provider、模型和已用时间；重试状态显示下一次尝试倒计时。
- 面板切换、列表刷新、工具完成和响应完成使用 120–200ms 的轻量过渡；动画只改变预留区域内的字符和样式，保持消息、输入框与状态栏位置稳定。
- 请求进行中保持 Ctrl-C 取消、滚动、详情展开等交互；状态切换由 operation ID 校验，忽略迟到的 tick、网络响应和已取消任务事件。
- TTY 默认启用动画；`--no-animation`、`TERM=dumb`、管道输出和 `--output json|jsonl` 使用静态状态事件。遵循 `NO_COLOR`，动画关闭时 loading 文本和耗时仍完整。
- 统一取消提示、确认提示和错误提示；终端 resize 时重新计算 viewport，并为输入框、进度条、spinner 和状态栏设置稳定尺寸。
- 保留 `--output text|json|jsonl`，方便脚本集成和回归测试。
- TUI 包含滚动历史、底部多行输入框、当前模式、Provider/模型、上下文占用、token/耗时状态栏。

**测试**：

- 使用可注入 clock 和固定 tick 序列测试每种 loading 状态、取消、重试、operation ID 去重和动画关闭模式。
- Provider 表单测试覆盖遮罩 Key、模型拉取成功/失败、列表搜索、手工模型 ID、字段校验和热更新结果。
- viewport snapshot 覆盖 Windows Terminal、常见 ANSI 终端、窄屏、resize、超长模型 ID 和中英文混排。
- 验证模型流、工具流与 spinner 并发时只有 Bubble Tea 渲染循环写终端，消息顺序稳定且 goroutine 可退出。

**验收标准**：所有网络、模型和工具等待阶段都有明确 loading 状态、动画和耗时；动画期间输入区与历史区保持稳定；终端能直观看出模型文本和工具调用；确认提示与流式输出顺序稳定；TTY 与管道输出均可用；颜色或动画关闭时信息仍完整。

### Phase 8：会话持久化与恢复

**目标**：支持中断、恢复和多 session 管理。

**设计选择**：采用 Eylu JSONL 事件日志作为事实来源，使用 JSON 快照加速加载。事件包含 schema 版本、session ID、时间、工作区、模式、turn、工具调用、驱动名称、opaque 驱动状态和错误。

**实现任务**：

- `eylu chat --session <id>` 创建或打开指定会话。
- `eylu chat --resume` 恢复最近会话；`eylu sessions list|show|delete` 管理会话。
- `/new` 追加 `SessionClosed` 事件，刷新当前快照，再创建带新 ID 的空 session；旧 session 保留在列表中。
- 事件追加写入，定期生成快照；崩溃恢复时重放快照后的事件。
- 保存 Skill catalog 快照与激活事件，包括名称、来源、入口、digest 和触发方；恢复时重新校验入口并明确报告内容变化或缺失。
- 写入采用临时文件、刷新和原子替换；清理策略按数量或磁盘大小配置。
- 保存标准化 usage、Provider 名称与 generation、模型、驱动、工作区、权限模式和 ContextLedger 快照，敏感值永不落盘。
- 工具输出可保存为附件文件，消息中仅保留引用和摘要。
- 提供 schema migration 入口，禁止静默读取未知版本。

**验收标准**：关闭程序后恢复历史、当前模式、Provider 引用和未完成任务；`/new` 生成新 session 且旧 session 可恢复；清除驱动私有状态后仍可用 Eylu transcript 重建请求；损坏尾部 JSONL 不影响已提交事件；多个 session 互相隔离；删除 session 前有确认。

### Phase 9：扩展与发布

**目标**：在核心稳定后扩展模型、并发和工具生态。

**候选功能**：

- 根据任务类型、成本、上下文窗口和 DriverCapabilities 在已有 Provider 间自动路由，保留用户固定选择模式。
- 对互不依赖的只读工具做并发执行，保留确定性的结果排序。
- 外部工具通过 JSON-RPC/MCP 或子进程协议接入，核心 registry 继续使用同一契约。
- MCP server 的 instructions、tool schema、resource 与 tool result 全部登记到 `ContextLedger` 的 MCP 分类，`/context` 可按 server 展开。
- Skill 安装、更新、签名验证、远程 registry 和团队级分发；本地 `SKILL.md` 运行时契约保持稳定。
- 增加 `goreleaser`、跨平台构建、签名、校验和发布说明。
- 增加性能指标：首 token 延迟、总耗时、工具成功率、上下文压缩次数、估算成本。

**验收标准**：新增工具无需修改 Agent Loop；新增模型驱动无需修改 Eylu protocol、Policy 和 session schema；并发只读工具在取消和失败时可收敛；发布包能在 Windows、Linux、macOS 启动并通过 smoke test。

## 5. 测试策略

### 5.1 测试层级

| 层级 | 目标 | 运行方式 |
|---|---|---|
| 单元测试 | 配置、消息、策略、路径、命令分类、ContextLedger、上下文压缩 | 每次提交 |
| Provider contract | CRUD、keyring 引用、模型目录、generation 热更新、原子回滚 | fake HTTP server + 临时配置目录 |
| Driver contract | Eylu 双向映射、流式聚合、错误映射 | fake HTTP server |
| Tool contract | schema、风险标签、结果结构、取消和超时 | 临时工作区 |
| Skill contract | 规范解析、目录优先级、渐进披露、资源边界、权限裁决 | 临时项目与用户目录 |
| UI 状态机 | loading、动画 tick、取消、resize、Provider 表单、静态降级 | 注入 clock + viewport snapshot |
| Agent transcript | 固定模型响应驱动多轮工具闭环 | 无网络、可回放 |
| 集成测试 | 已配置模型驱动和兼容网关的最小 smoke test | 手动或受保护 CI |
| 人工验收 | 四种模式、确认交互、终端显示 | Windows/macOS/Linux |

### 5.2 必测失败路径

- 凭据缺失、认证失败、限流、服务端错误、连接中断。
- Provider 配置为空、Key 存储失败、`/v1/models` 缺失或响应畸形、手工模型 ID、热更新校验失败、活动 Provider 删除和并发配置写入。
- 流式响应中途取消、外部响应未完成、驱动映射为 `StopLength` 和未知事件。
- 工具输入 JSON 错误、未知工具、重复 Eylu tool call ID。
- 工作区路径穿越、符号链接、权限不足、文件编码错误。
- Skill frontmatter 损坏、同名冲突、项目待信任、资源越界、入口内容变化和激活去重。
- 命令超时、非零退出、输出过大、子进程树未退出。
- 用户拒绝确认、会话损坏、快照版本不兼容。
- `/new` 快照写入失败、`/context` 分类遗漏或求和偏差、上下文窗口未知、动画迟到事件和终端 resize 风暴。

### 5.3 CI 质量门槛

```bash
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

发布前增加 `staticcheck`、跨平台构建和最小 smoke test。真实 API 测试使用独立凭据、预算上限和显式开关。

## 6. 日志、可观测性与故障处理

统一结构化日志字段：`timestamp`、`session_id`、`request_id`、`turn`、`mode`、`event`、`provider_name`、`provider_generation`、`model`、`ui_state`、`context_category`、`tokens`、`tool_name`、`call_id`、`skill_name`、`skill_source`、`skill_digest`、`duration_ms`、`bytes`、`exit_code`、`error_code`。

- stdout 保留面向用户的文本，stderr 输出诊断，JSONL 模式输出结构化事件。
- 凭据、Authorization、Cookie、环境变量值和疑似密钥内容统一脱敏。
- 每次模型请求和工具执行都有可关联 ID。
- 失败结果包含用户可操作的下一步，例如重新确认、缩小范围、继续请求或切换模式。
- 进程退出前等待日志刷新，取消时回收所有 goroutine 和子进程。

## 7. 关键风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| 工具循环失控 | 成本增加、终端卡住 | 最大轮数、总 token 预算、单工具超时、全局取消 |
| 模型生成危险命令 | 文件或系统损坏 | 工作区边界、命令分类、黑名单、高危二次确认 |
| Provider 热更新竞态 | 请求混用地址、凭据或 DriverState | 不可变快照、generation、请求级捕获、串行写入、失败回滚 |
| Provider 凭据泄露 | API Key 出现在配置、日志或终端历史 | password input、系统 keyring、TOML 引用、统一脱敏、memory 回退 |
| TUI 动画与异步输出竞争 | 画面错位、输入阻塞、CPU 占用升高 | 单写入循环、固定帧率、operation ID、稳定尺寸、静态降级 |
| 第三方 Skill 供应链 | 指令注入、恶意脚本、数据外传 | 项目信任、来源展示、内容 digest、统一 Policy、远程安装延后交付 |
| Skill 内容在会话中变化 | 行为漂移、回放结果失真 | 激活 digest、恢复时复核、变化诊断、显式重新激活 |
| 工具输出过大 | 上下文快速膨胀 | 字节上限、摘要、附件引用、压缩事件 |
| 流式协议处理错误 | 工具结果被外部服务拒绝、历史损坏 | ModelDriver 聚合测试、原始块保留、transcript 回归 |
| Windows/Unix shell 差异 | 命令行为不一致 | `ShellAdapter`、平台集成测试、默认展示实际 shell |
| 会话含敏感代码或凭据 | 本地泄露 | 脱敏、用户目录权限、可选清理和附件引用 |
| 外部 API/SDK 版本变化 | 构建或运行失败 | ModelDriver 隔离、依赖锁定、contract test、版本升级记录 |

## 8. 里程碑与交付顺序

| 里程碑 | 覆盖 Phase | 交付物 |
|---|---|---|
| M0 可运行骨架 | 0 | Provider 首次引导、模型发现、单次 chat、fake server、CI 基线 |
| M1 可对话 | 1 | 多轮历史、流式输出、`/new`、`/context`、Provider 热管理 |
| M2 可执行 | 2 | 三个基础工具、完整闭环、工具日志 |
| M3 可编辑 | 3 | 精确编辑、搜索、目录探索、diff |
| M4 可控 | 4 | 四种权限模式、命令策略、确认和审计 |
| M5 可复用 | 5 | Skill 发现、激活、资源读取、校验与审计 |
| M6 可持续 | 6 | 分类 `/context`、token 预算、摘要、项目地图、Skill 上下文保护 |
| M7 可用 | 7 | Bubble Tea TUI、动画 loading、Provider 表单、上下文视图、脚本输出格式 |
| M8 可恢复 | 8 | session、`/new` 边界、Provider 引用、Skill 激活恢复、迁移入口 |
| M9 可扩展 | 9 | Provider 自动路由、并发只读、MCP 上下文、Skill 分发、发布包 |

每个里程碑完成后保留一个可运行 tag，并记录已知限制、配置变更、transcript 样例和升级注意事项。

## 9. Definition of Done

一个 Phase 只有同时满足以下条件才算完成：

- 功能在文档范围内实现，未引入未记录的行为变化。
- 单元测试和对应集成/transcript 测试通过。
- 错误、取消、超时和资源上限都有测试覆盖。
- 日志包含足够的关联信息，敏感信息完成脱敏。
- 核心包依赖图只指向 Eylu protocol 和标准接口，供应商 SDK 仅出现在对应 driver 包。
- Skill 相关变更通过规范 fixture、路径边界、权限矩阵和 transcript 回放测试。
- Provider 与 TUI 相关变更通过热更新、凭据脱敏、模型发现、loading 状态机和静态降级测试。
- README 或命令帮助同步更新，示例可复制运行。
- 验收标准由人工或自动化测试逐条勾选。
- 变更记录包含配置、数据格式和兼容性影响。

## 10. 建议的首轮实现顺序

1. Phase 0 先交付 ProviderManager、凭据存储、首次引导、模型发现和单次 chat，确保空配置也有完整启动路径。
2. Phase 1 交付多轮流式会话、`/new`、基础 `/context` 和 Provider 运行时管理，固定 session 与 provider generation 语义。
3. 立即实现 Phase 2 的工具 registry、Agent Loop 和基础资源限制，工具数量保持在三个。
4. 将路径边界、命令超时、输出截断和日志脱敏作为 Phase 2 的强制基线。
5. 交付 Phase 3 后进入 Phase 4，先完善统一策略，再增加自动模式和 Full 模式。
6. 在统一策略稳定后交付 Phase 5，先完成 Skill 发现、激活和资源边界，再接入脚本工作流。
7. Phase 6 完成全分类 ContextLedger、压缩与 `/context` 展示，随后由 Phase 8 固化 `/new`、Provider generation、会话事件和 digest 恢复语义。
8. Phase 7 完善动画 TUI 与 Provider 表单，Phase 9 扩展自动路由、MCP 和发布能力，保持核心 Agent Loop 无 UI 依赖。

## 11. 外部协议与规范参考

模型协议资料用于实现 `internal/driver/*`，Eylu 核心协议和配置保持独立：

- [OpenAI Function calling](https://developers.openai.com/api/docs/guides/function-calling)
- [OpenAI Streaming API responses](https://developers.openai.com/api/docs/guides/streaming-responses)
- [OpenAI Using tools](https://developers.openai.com/api/docs/guides/tools)
- [OpenAI Responses API reference](https://developers.openai.com/api/reference/resources/responses/methods/create)
- [OpenAI Chat Completions API reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
- [OpenAI Models list API reference](https://developers.openai.com/api/reference/resources/models/methods/list)

Skill 实现遵循以下开放规范和客户端接入指南：

- [Agent Skills Specification](https://agentskills.io/specification)
- [How to add skills support to your agent](https://agentskills.io/client-implementation/adding-skills-support)

终端与凭据实现参考：

- [Bubble Tea](https://github.com/charmbracelet/bubbletea)
- [Bubbles v2 spinner](https://pkg.go.dev/charm.land/bubbles/v2/spinner)
- [go-keyring](https://github.com/zalando/go-keyring)
