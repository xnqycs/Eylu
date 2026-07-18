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

配置优先级为命令行参数、`EYLU_*` 环境变量、工作区 `.eylu/config.toml`、用户目录 `~/.eylu/config.toml`、默认值。配置文件仅保存凭据引用；交互式首次引导会优先保存到系统 keyring。

当前多轮 transcript、已关闭 session 和 DriverState 保存在进程内；Phase 8 的事件日志与快照会提供跨进程恢复。

## 开发质量门槛

```bash
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```
