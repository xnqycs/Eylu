# Eylu

Eylu 是一个面向本地代码库的 Go 终端编程 Agent。核心协议、模型驱动、工具、权限、上下文和会话持久化保持解耦，兼容 OpenAI Responses 风格的 HTTP 网关。

## 构建

```bash
go build -o eylu .
go test ./...
```

## 快速开始

使用环境变量保存凭据，并通过运行时参数发起一次请求：

```powershell
$env:EYLU_API_KEY="your-key"
go run . chat "你好" --base-url "https://api.openai.com/v1" --model "your-model"
```

持久化 Provider 配置：

```powershell
$env:EYLU_API_KEY="your-key"
go run . providers add work --base-url "https://api.openai.com/v1" --model "your-model" --credential-type env --credential-env EYLU_API_KEY
go run . providers models --provider work
go run . chat "检查当前项目" --provider work
```

兼容端点可在 Provider 中选择两种 adapter：

- `openai_responses`：使用 `/v1/responses` 和类型化 SSE 事件。
- `openai_chat`：使用 `/v1/chat/completions` 和 Chat Completions 流。

在终端直接运行 `go run . chat` 会进入多轮交互会话。可用命令包括 `/help`、`/new`、`/context`、`/providers`、`/provider add|edit|delete|use`、`/model` 与 `/quit`。文本输出实时呈现模型增量；`--output json` 输出完整响应对象。

Agent 默认提供以下工具：

- `read_file`：读取工作区内 UTF-8 文件，限制读取字节并拒绝路径穿越。
- `write_file`：在确认后原子创建或替换文件，父目录创建必须由模型显式声明。
- `bash`：在确认后通过平台 shell 执行命令，限制环境、超时与输出大小。
- `edit_file`：按预期匹配次数精确替换，保留权限与换行风格并返回 unified diff。
- `search_code`：在 Git ignore 语义下进行字面量或 RE2 正则搜索。
- `list_directory`：从共享仓库索引生成稳定、限深的目录树。

只读工具直接执行；写入与命令工具会在 TTY 中确认。脚本化运行可使用 `--yes` 明确授权本次请求中的确认项：

```powershell
go run . chat "读取 go.mod 并执行 go test ./..." --provider work --yes
```

权限模式可通过 `--mode` 或交互命令 `/mode` 切换：

| 模式 | 行为 |
|---|---|
| `manual` | 读取自动执行；写入和命令确认；高危操作确认两次。 |
| `plan` | 读取与已分类的只读命令执行；写入和其他命令拒绝；最终生成实施计划。 |
| `auto` | 写入与命令白名单自动执行；未知命令确认；高危操作确认两次。 |
| `full` | 普通操作自动执行；高危操作显示警告并确认。 |

命令分类会检查链式命令、重定向、命令替换、阻止规则和高危模式。`read_only_commands`、`auto_allow_commands`、`dangerous_commands`、`blocked_commands` 与 `shell_environment` 可在 TOML 中配置。

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

交互会话支持 `/skills` 和 `/skill <name>`。Skill 的 `allowed-tools` 仅作为提示和审计信息，工具执行继续服从当前权限模式。

配置优先级为命令行参数、`EYLU_*` 环境变量、工作区 `.eylu/config.toml`、用户目录 `~/.eylu/config.toml`、默认值。配置文件仅保存凭据引用；交互式首次引导会优先保存到系统 keyring。

当前多轮 transcript、已关闭 session 和 DriverState 保存在进程内；Phase 8 的事件日志与快照会提供跨进程恢复。

## 开发质量门槛

```bash
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```
