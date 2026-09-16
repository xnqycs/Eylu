# Eylu

[简体中文](README.md) | English

A terminal programming agent for local codebases. Eylu understands code in your workspace, invokes tools, executes plans, and preserves sessions while supporting HTTP gateways compatible with the OpenAI Responses API or Chat Completions API.

[Download](https://github.com/xnqycs/Eylu/releases) · [Changelog (Chinese)](CHANGELOG.md) · [Release guide (Chinese)](RELEASING.md) · [License](LICENSE)

<p align="center">
  <img src="docs/assets/eylu-tui.png" alt="Eylu TUI startup screen" width="1100">
</p>

## Why Eylu

| Capability | What you get |
|---|---|
| Local repository context | Automatically collects the project structure, Git status, and relevant files while keeping references inside the workspace boundary |
| Complete agent loop | Supports streamed model output, multi-turn tool calls, task lists, questions, and execution auditing |
| Controlled tool permissions | `manual`, `plan`, `auto`, and `full` modes cover review, planning, and automated execution workflows |
| Persistent sessions | Saves prompts, task state, and the context ledger, with compression and session recovery |
| Multi-provider routing | Selects models by task, capability, context window, priority, and cost |
| Extensible capabilities | Supports Agent Skills, signed Skill registries, and MCP stdio, Streamable HTTP, and SSE servers |

Eylu provides a full-screen TUI and can also integrate with scripts and automation through text, JSON, or JSONL output.

## Installation

### Download a prebuilt release

Download the archive for your system from [GitHub Releases](https://github.com/xnqycs/Eylu/releases):

| System | x64 | ARM64 |
|---|---|---|
| Windows | `Eylu_<version>_Windows_amd64.zip` | `Eylu_<version>_Windows_arm64.zip` |
| Linux | `Eylu_<version>_Linux_amd64.tar.gz` | `Eylu_<version>_Linux_arm64.tar.gz` |
| macOS | `Eylu_<version>_Darwin_amd64.tar.gz` | `Eylu_<version>_Darwin_arm64.tar.gz` |

Extract the archive and check the version:

```powershell
# Windows
.\eylu.exe version
```

```bash
# Linux / macOS
chmod +x eylu
./eylu version
```

The remaining examples assume that `eylu` is available in `PATH`. On Windows, add the extracted directory to your user `Path`. On Linux and macOS, install the executable into your user command directory:

```bash
mkdir -p "$HOME/.local/bin"
install -m 755 eylu "$HOME/.local/bin/eylu"
```

Make sure `$HOME/.local/bin` is included in `PATH`.

Each archive contains only the main executable. Releases also include a SHA-256 checksum file and a Sigstore bundle. Replace the example version and archive name below with the release you downloaded.

Check the archive hash on Linux or macOS and compare it with the matching entry in the checksum file:

```bash
VERSION=1.0.0-rc.1
ARCHIVE="Eylu_${VERSION}_Linux_amd64.tar.gz"
sha256sum "$ARCHIVE"
grep " $ARCHIVE$" "Eylu_${VERSION}_checksums.txt"
```

On Windows PowerShell:

```powershell
Get-FileHash .\Eylu_1.0.0-rc.1_Windows_amd64.zip -Algorithm SHA256
Select-String "Eylu_1.0.0-rc.1_Windows_amd64.zip" .\Eylu_1.0.0-rc.1_checksums.txt
```

Verify the signature attached to the checksum file:

```bash
VERSION=1.0.0-rc.1
cosign verify-blob \
  --bundle "Eylu_${VERSION}_checksums.txt.sigstore.json" \
  --certificate-identity "https://github.com/xnqycs/Eylu/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "Eylu_${VERSION}_checksums.txt"
```

The [release guide (Chinese)](RELEASING.md#5-发布后验证) covers the complete post-release verification procedure.

### Build from source

Go 1.25.8 or later is required:

```bash
git clone https://github.com/xnqycs/Eylu.git
cd Eylu
go build -trimpath -o eylu .
go test ./...
```

## Quick Start

### 1. Start the TUI

Open the project you want Eylu to work on and run:

```bash
cd path/to/your-project
eylu
```

On the first run, Eylu starts the provider setup flow:

1. Confirm the provider name and API base URL.
2. Enter the API key. Input is hidden while you type.
3. Select an automatically discovered model or enter a model ID.
4. Confirm the model context window.

After setup, Eylu opens the full-screen TUI. Future launches use the saved provider directly. Provider settings and the API key are stored in `~/.eylu/config.toml`.

### 2. Start a conversation

Describe the task in the input area at the bottom and press `Enter`. Eylu reads the current workspace context and shows tool execution, task progress, and context usage.

Common interactive commands:

```text
/help       Show available commands
/new        Start a new session
/tasks      Show the full task list
/context    Inspect context usage
/providers  Manage providers
/model      Switch models
/effort     Change the reasoning effort
/skills     Inspect Skills
/mode       Change the permission mode
/quit       Exit Eylu
```

### Environment variables and command-line setup

Environment variables work well for temporary credentials and automated environments:

```powershell
# Windows PowerShell
$env:EYLU_API_KEY="your-api-key"
```

```bash
# Linux / macOS
export EYLU_API_KEY="your-api-key"
```

Create a Responses provider in advance:

```bash
eylu providers add work --base-url "https://api.example.com/v1" --model "your-model-id"
eylu providers list
```

Specify the adapter for a Chat Completions compatible gateway:

```bash
eylu providers add work-chat --adapter openai_chat --base-url "https://api.example.com/v1" --model "your-model-id"
```

Run `eylu` after configuration to enter the TUI. `EYLU_API_KEY` overrides the key stored in the provider for each request.

### Hosted Web Search and Web Fetch

Eylu models `web_search` and `web_fetch` separately from function tools and resolves capabilities by `catalog_provider + adapter + model`. Recognized providers with supported Web capabilities publish the executable tools automatically. Compatible gateways can declare support through `web_capabilities`. Web permission defaults to `allow`, so search and fetch run directly; explicitly set `ask` or `deny` to require approval or disable access. GPT models behind compatible Responses relays hand same-round `queries` to Eylu for controlled fan-out, with up to 10 concurrent queries per batch and one result merged in original query order; `max_uses` continues to count model tool calls. The TUI expands batched queries into individual child items and shows the actual query, opened URL, and sources. Its collapsed view keeps the five newest items visible, renders hidden entries as `▸ … +N hidden`, and toggles the full history when that row is clicked.

| Adapter | Native Web mapping |
|---|---|
| `openai_responses` | Responses hosted search/fetch with OpenAI, xAI, and OpenRouter dialects |
| `openai_chat` | Chat hosted search with OpenRouter, Groq Compound, and Qwen/DashScope options |
| `anthropic_messages` | Versioned server tools with bounded `pause_turn` continuation inside one request |
| `gemini_interactions` | `google_search` and `url_context` |
| `mistral_conversations` | Standard and premium Web search |
| `perplexity_agent` | `web_search` and `fetch_url` |

Core CLI configuration:

```bash
eylu providers edit work --catalog-provider openai --web-permission allow --web-search auto --web-fetch auto --web-max-uses 5 --web-context-size medium
```

The full TOML form supports capability overrides, delegated fallback, and MCP client fallback:

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

`execution` accepts `auto`, `hosted`, `delegated`, and `client`. `auto` prefers the active model's hosted capability and follows an explicit `fallback` to another configured provider or a named MCP tool. MCP fetch requires `trusted_network_boundary = true`. Eylu validates the initial HTTP(S) URL, credentials, domain rules, and resolved public addresses; the MCP server is responsible for applying equivalent redirect and DNS checks inside that trusted boundary.

Hosted execution sends the query, URL, domain rules, location, and allowlisted provider options to the active provider. Delegated execution sends them to the target provider. Client execution sends canonical `query` or `url` input to the named MCP server. Web content is marked as untrusted input. Activities, citations, Web tokens, cost, and backend details are projected into protocol events, sessions, JSON/JSONL, metrics, and audit records. Existing credential redaction still applies to logs.

## Common Workflows

### One-shot request

```bash
eylu --no-tui "Inspect the current project and list its risks"
```

### Resume a session by ID

```bash
eylu --resume auth-review
eylu chat --resume auth-review
```

`--resume <session-id>` loads an existing session from the current workspace exactly. An invalid, missing, damaged, or cross-workspace ID returns a non-zero exit code and leaves session storage unchanged. TUI and interactive `--no-tui` sessions show restored messages and tool history at the latest content; one-shot calls with a prompt continue to print only the new response. Interactive text sessions print a directly executable resume command when they exit.

`--session <id>` keeps its open-or-create semantics for named sessions:

```bash
eylu "Review the authentication module" --session auth-review
eylu --resume auth-review "Continue the fix"
eylu sessions list
eylu sessions show auth-review --output json
```

### Structured output

```bash
eylu --no-tui --output jsonl "Inspect the project and run its tests"
```

JSONL emits routing, context, model, tool audit, and final response events one line at a time, making it suitable for log collection and automation.

### Automatic provider selection

Declare the tasks and priority for a provider:

```bash
eylu providers add coding --base-url "https://api.example.com/v1" --model "coding-model" --routing-task coding,debugging,testing --routing-priority 20
```

Send a request through automatic routing:

```bash
eylu --route auto --task review "Review the current changes and run the tests"
```

The router considers task matching, model capabilities, the effective context window, priority, and configured cost, then reports why it selected the provider.

## Permission Modes

| Mode | Behavior |
|---|---|
| `manual` | Reads run automatically; writes and commands wait for approval; high-risk operations require a second confirmation |
| `plan` | An isolated planning agent uses read-only capabilities, then lets you choose how to execute the plan |
| `auto` | Allowlisted writes and commands run automatically; unknown commands wait for approval; high-risk operations require a second confirmation |
| `full` | Regular operations run automatically; high-risk operations display a warning and wait for approval |

Select a mode at startup:

```bash
eylu --mode plan
```

In the TUI, press `Shift+Tab` to cycle through all four modes. A mode change made during a run takes effect on the next turn.

### Read-only command classification and policy composition

`plan` mode only allows commands that can be proven free of side effects, and the `auto` mode allowlist prefix does not bypass that check.

- A command line is parsed segment by segment, and it is classified read-only only when every segment is proven side-effect free.
- Arguments are validated per command. `find -delete`, `-exec`, `-execdir`, `-fprint`; `git branch -d`, `-D`, `-m`, `-M`, `-c`, `-C`, `--delete`, `--set-upstream-to`; `git diff`/`git log`/`git show --ext-diff`, `--textconv`, `--output`, `--show-signature`; `git grep -O`; and `git -c`, `--config-env`, `--exec-path`, `--paginate` are reported as dangerous rather than read-only.
- Short options are case sensitive, so `pwd -P` stays read-only while `git grep -O` is dangerous.
- Inputs that cannot be interpreted reliably fall back to unknown: unterminated quotes, variable or command substitution (`$VAR`, `$(...)`), pipes, redirections, backticks, and any argument to a command from `read_only_commands` that has no built-in argument rules. `plan` mode denies unknown, while `manual` and `auto` require confirmation according to their own rules.
- Policy is layered: non-relaxable prohibitions, mode defaults, tool-domain policy (such as the independent web permission), and explicit approvals. An explicit global prohibition is never relaxed by tool-level policy, and a zero or unrecognized decision is always treated as a denial.
- Audit records keep the mode, the matched rule, the final decision, and the override source.

Behavior change: argument forms that were previously allowed by a command prefix alone (`find . -delete`, `git branch -D`, `git diff --ext-diff`) are now reported as dangerous or unknown.

Command classification is application-level policy, not an operating-system sandbox: `working_directory` only selects where a command starts, it does not restrict which paths the command can reach.

## Skills and MCP

Eylu discovers Agent Skills in this order:

```text
<workspace>/.eylu/skills
<workspace>/.agents/skills
~/.eylu/skills
~/.agents/skills
```

Project-level Skills require workspace trust. Diagnose them before activation:

```bash
eylu skills list
eylu skills validate ".agents/skills/code-review"
eylu skills diagnose --output json
```

MCP servers support the `stdio`, `streamable_http`, and `sse` transports and are configured in Eylu TOML:

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

An `sse` server uses the same `url`, header, and OAuth fields. Static headers can be configured directly with `headers = { Authorization = "Bearer token" }`; sensitive values can also be injected through `environment_headers`, `bearer_token_environment`, or OAuth. The compatibility fields `disabled`, `timeout_seconds`, and `read_only_tools` remain supported. Startup, call, and OAuth/interaction timeouts default to 60, 60, and 30 seconds. Up to four servers connect concurrently. Each Streamable HTTP handshake POST and tool discovery receive their own startup timeout; HTTP clients retain server cookies and send a stable User-Agent. A server becomes connected as soon as tools are ready, while logging level, resources, resource templates, and prompts load in the background. Optional catalog failures are recorded as diagnostics while connected tools remain available. Transient connection failures retry up to three times; authentication, configuration, and user cancellation failures stop immediately. After retries are exhausted, use `reconnect`; session cleanup on exit has a two-second bound.

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

After the TUI starts, a spinner below the banner shows MCP loading progress and its row is cleared when loading reaches either terminal state. Enter `/mcp` to open the server list and detail panel directly. Use the left/right arrow keys or number keys to switch between details, tools, resources, and prompts. The Tools tab shows a selectable list; press Enter for tool details and Esc to return. The first TUI request reuses the MCP manager created at startup. Connection errors appear in the content area and remain in chat history; after automatic retries are exhausted, an HTTP 502 includes a manual reconnect hint. Background diagnostics do not write through the input area. Catalog notifications atomically refresh the tool registry, context, and cache fingerprint. OAuth credentials are stored in `~/.eylu/mcp_credentials.json` with file locking, atomic replacement, and platform-specific permission hardening.

MCP environment variables are forwarded through a name allowlist. Read-only tools must also be declared explicitly in the local configuration.

## Configuration and Data

Configuration precedence:

```text
command-line arguments > EYLU_* environment variables > <workspace>/.eylu/config.toml > ~/.eylu/config.toml > defaults
```

Common paths:

| Content | Default location |
|---|---|
| User configuration | `~/.eylu/config.toml` |
| Project configuration | `<workspace>/.eylu/config.toml` |
| Sessions and model cache | `~/.eylu/state/` |
| Project Skills | `<workspace>/.eylu/skills/`, `<workspace>/.agents/skills/` |

`EYLU_WORKSPACE` overrides the current workspace, and `EYLU_STATE_DIR` changes the state directory. Session files exclude API keys, provider headers, and other credentials.

### Parallel tool calls

Eylu asks the model to return independent tool calls in one turn and schedules them with file, directory, and session-state awareness. Read-only tools, classified read-only Bash commands, and writes to different files may run concurrently. Writes to the same file plus interactive or session-state operations stay ordered.

```toml
max_parallel_tools = 4
```

The default concurrency limit is `4`. Set it to `1` for serial execution, or override it temporarily with `EYLU_MAX_PARALLEL_TOOLS`. Explicitly configured read-only MCP tools can join concurrent batches; other MCP tools execute exclusively.

### Cancellation, failure, and batch termination

- The scheduler re-checks cancellation before starting each call, after `OnStart` returns, and again after a resource claim is granted, so a call never starts once cancellation has been observed. A queued waiter is removed from the resource coordinator when it is cancelled.
- Results already obtained are preserved when a request ends. A write that committed successfully is never rewritten as "did not execute" because of a later cancellation, while the request itself still reports the cancellation.
- A tool failure and a request-infrastructure failure behave differently. An ordinary tool error is left to the model to adjust to; an approval-channel failure or a scheduler that cannot make progress aborts the whole preflight batch, so calls already approved in that batch do not run either, and the request reports a failure instead of a user interruption.
- When several errors coincide, the first substantive failure stays the leading error and the cancellation cause is preserved beside it, so `errors.Is` holds for both.
- A tool timeout is a cooperative cancellation: the executor relies on the tool honouring context cancellation and never starts an unreclaimable goroutine to fake a hard timeout. Untrusted or non-cooperative plugins would need process isolation, which is a separate enhancement.
- Every call produces at most one start event and one terminal event, and at most one audit record.
- Every tool batch reports a host-owned control state: `continue` (ordinary failure, left to the model), `interrupt_request` (user refusal without a reason), `cancel_request` (the request context was cancelled), and `abort_request` (approval-channel or execution infrastructure failure). Call states are `succeeded`, `failed`, `rejected`, `cancelled`, `not_executed`, and `outcome_unknown`.
- Control states are produced by the executor and the approval layer; they never read tool content, MCP annotations, or result metadata. An external tool cannot interrupt or abort a host request by forging `interrupt_request` or similar fields.
- Web fan-out aggregation only handles content and display: it keeps the parent call ID, keeps each child's terminal state, and keeps the content of the queries that succeeded. Control never passes through the aggregation layer, so single-query and multi-query calls share one control semantics. A query that never ran, or that was refused, is not projected as a search that executed.
- For compatibility with older UI, results still publish transitional metadata such as `interrupt_request`, `approval_rejected`, `rejection_reason`, and `batch_cancelled`; the new control logic does not read them.

### Tool call commit and terminal closure

- A model response is validated before it is committed: turn role and part structure, empty tool call IDs, duplicates inside one response, argument JSON validity, call ID uniqueness in the session history, and consistency between `Stop` and the tool calls. A response that fails validation is never written to the transcript; the user message stays and the untrustworthy driver state is dropped.
- Every committed tool call must reach a terminal state. Budget exhaustion, tool registry refresh failure, web tool resolution failure, cancellation and interruption all close the calls that never executed with `not_executed`; a success that already happened is never rewritten.
- A response that reports completion while returning tool calls is a protocol contradiction: it is returned as a protocol error instead of being treated as a normal completion, and it never strands a call.
- A truncated (`length`) response keeps its partial content, its tool calls are not executed, and they are closed with `not_executed`.
- The stored transcript is left as it is. When a model request is built, a historical call with no recorded result is closed with `outcome_unknown` and is never replayed automatically; the diagnostic is available through `RecoveryNotes`.
- `Send` and `Adopt` do not run tools, so the calls they record are closed too.

### Code slice references and context trimming

Code slice deduplication — replacing a repeated read with a stable reference — only applies to a fragment whose **body is complete and whose line range is reliable**:

- Trimming for the context window keeps the head and tail and inserts a summary marker. The trimmed copy is explicitly marked incomplete (`context_truncated`, `Truncated`) and is therefore no longer treated as a canonical fragment.
- An incomplete large fragment does not overwrite a smaller complete fragment, does not make later reads degrade into references, and does not redirect existing references; otherwise the model would receive a reference to a body that does not contain the range.
- A read the tool itself declared incomplete (`lines_complete = false`, for example when it reached `max_read_lines`) is not canonical either.
- A changed file hash never references an older generation of the content.
- The original transcript is never modified, and omitted text never becomes a basis for deduplication. Token savings may suffer; content correctness may not.

### Session event log and snapshot

The event log (`events.jsonl`) and the snapshot (`snapshot.json`) track their progress independently, and both describe the session state:

- A confirmed append is durable: log progress advances immediately. A later snapshot save failure only marks the snapshot as behind; it never rolls the log progress back.
- The next sync does not re-send an event the log already accepted. It retries the snapshot save and appends only genuinely new events. Turns, prompts, skills, runtime, context, driver state and error records share one progress mechanism.
- State events (runtime, context, agentTasks, driverState, error) are appended only when their payload changes, so a repeated sync produces no duplicate events.
- When the snapshot is missing or behind, the event log alone restores the session, and recovered turns are not duplicated.
- Stable event IDs, uncertain append results (Write/Sync/Close failures) and schema migration belong to a later stage (PR-10).

### Code context and background subagents

`read_file` accepts 1-based inclusive `start_line` and `end_line` ranges and returns stable file and slice hashes. `search_code` shares the session's incremental code index, supports pagination, and deduplicates overlapping code slices before model calls.

The `agent` tool launches `search` or `general` subagents. Every task is forced to run in the background and returns a `task_id` immediately; legacy `run_in_background=false` input is ignored. `task_output` returns an immediate snapshot without waiting or consuming the completion notification, while `task_stop` cancels the current turn and clears queued messages. When a background task reaches a terminal state, an idle parent conversation continues automatically; a running parent receives the result before its next model call. Results are injected exactly once as `<agent_notification>`, with concurrently completed tasks delivered together.

`search` exposes only `search_code`, `read_file`, and `list_directory` and returns a structured report. `general` inherits the parent context, model, reasoning effort, permission mode, MCP catalog, and activated Skills. It owns a separate Conversation and serial message queue, and recursive `agent` calls are disabled. All subagents share the workspace, resource coordinator, and `max_parallel_agents` limit. Writes to the same path remain ordered; a general subagent's `write_file` creates new files only, while existing files require a fresh read and a precise `edit_file` call.

Enter `/agents` or `/agents <filter>` in the TUI to select an agent from the current Session. Enter opens its full-screen conversation and sends follow-up messages, `s` stops an active task, and Esc restores the main conversation draft and scroll position. Approvals inherit the parent mode and appear in FIFO order with the agent source. On exit, Eylu cancels and waits for active agents and persists terminal transcripts; restored agent conversations are view-only.

```toml
max_parallel_agents = 2
code_context_cache_bytes = 67108864
max_read_lines = 2000
code_index_workers = 4

[search_agent]
max_turns = 8
timeout_seconds = 120
# provider = "fast-model" # inherit the active provider when omitted
# model = "model-id"      # inherit the active model when omitted
```

The matching environment variables are `EYLU_MAX_PARALLEL_AGENTS`, `EYLU_CODE_CONTEXT_CACHE_BYTES`, `EYLU_MAX_READ_LINES`, and `EYLU_CODE_INDEX_WORKERS`.

## Terminal Compatibility

- An interactive TTY starts the full-screen Bubble Tea interface by default.
- `--no-animation` keeps the static theme and disables animation.
- `--no-tui` uses the line-oriented interface.
- `NO_COLOR` removes ANSI colors.
- `TERM=dumb`, pipes, and structured output automatically use the static rendering path.

## Project Documentation

- [CHANGELOG.md](CHANGELOG.md): version history (Chinese)
- [RELEASING.md](RELEASING.md): versioning, signing, CI, and recovery procedures (Chinese)
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md): third-party components and applicable terms
- [docs/go-terminal-agent-development-plan.md](docs/go-terminal-agent-development-plan.md): architecture and phased development history (Chinese)

## Development and Verification

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

CI runs tests, native builds, and smoke tests on Linux, Windows, and macOS. Release tags also produce six platform archives, SHA-256 checksums, and Sigstore signatures.

## License

Eylu is released by xnqycs under the [Apache License 2.0](LICENSE). See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for third-party components and their applicable terms.
