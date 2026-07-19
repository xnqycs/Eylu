# Changelog

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
