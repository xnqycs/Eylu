# Changelog

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
