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

配置优先级为命令行参数、`EYLU_*` 环境变量、工作区 `.eylu/config.toml`、用户目录 `~/.eylu/config.toml`、默认值。配置文件仅保存凭据引用；交互式首次引导会优先保存到系统 keyring。

## 开发质量门槛

```bash
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```
