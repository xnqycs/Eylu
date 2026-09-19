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

Go 1.25.13 or later is required:

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
- **The pending set is queryable.** While a request runs, the `Conversation` keeps an explicit set of its committed calls (`PendingCalls()` / `OpenPendingCalls()`): each entry carries the call ID, tool, parent call ID, the turn and round it belongs to, whether the executor accepted it (`prepared`), and its terminal state (empty while it is open). A call enters the set when its turn is committed, is marked `prepared` when the executor accepts it, and carries a state when its result is recorded. **Closing uses this set** instead of re-deriving the answer by scanning the transcript; the scan survives only on the recovery path, where there is no request to have tracked anything.
- A request always ends with the set empty, and the run report publishes `pending_at_end` plus a `warnings` entry when it does not: an open call at the end is a **defect signal**, not something left for the next crash to reveal. `prepared=false` is strong evidence that the call has not produced a side effect yet.

### Code slice references and context trimming

Code slice deduplication — replacing a repeated read with a stable reference — only applies to a fragment whose **body is complete and whose line range is reliable**:

- Trimming for the context window keeps the head and tail and inserts a summary marker. The trimmed copy is explicitly marked incomplete (`context_truncated`, `Truncated`) and is therefore no longer treated as a canonical fragment for its whole declared range.
- The trim keeps **whole lines** and records the line ranges that survived (`retained_ranges`): a fragment is canonical only for the lines it really holds. A target range that spans the omitted middle is never deduplicated, and its body is provided instead.
- An incomplete large fragment does not overwrite a smaller complete fragment, does not make later reads degrade into references, and does not redirect existing references.
- A read the tool itself declared incomplete (`lines_complete = false`, for example when it reached `max_read_lines`) is not canonical for its whole declared range either.
- When no whole line fits — a very long single line — the body is cut inside the line and reports no retained range, because a partially kept line can never back a reference.
- A changed file hash never references an older generation of the content.
- The original transcript is never modified, and omitted text never becomes a basis for deduplication. Token savings may suffer; content correctness may not.

The benefit is quantified: twenty repeated reads of the same 40-line region drop from 6000 to 718 code-slice tokens (about 88% saved), asserted by `TestDeduplicationQuantifiesItsTokenSavings`, with `BenchmarkCodeSliceDeduplication` providing a timing baseline.

### Session event log and snapshot

The event log (`events.jsonl`) and the snapshot (`snapshot.json`) track their progress independently, and both describe the session state:

- A confirmed append is durable: log progress advances immediately. A later snapshot save failure only marks the snapshot as behind; it never rolls the log progress back.
- The next sync does not re-send an event the log already accepted. It retries the snapshot save and appends only genuinely new events. Turns, prompts, skills, runtime, context, driver state and error records share one progress mechanism.
- State events (runtime, context, agentTasks, driverState, error) are appended only when their payload changes, so a repeated sync produces no duplicate events.
- When the snapshot is missing or behind, the event log alone restores the session, and recovered turns are not duplicated.
- Stable event IDs, uncertain append results (Write/Sync/Close failures) and schema migration belong to a later stage (PR-10).
- **Write count.** The intents of N side-effecting calls in one batch are merged into a single append, written before any call of the batch starts, so "the intent precedes the side effect" is untouched. Completions stay one write per call: merging them would save another N writes, but it would push the record of something that already happened past the point where the process can die, and for an effect with no file to inspect afterwards that turns a known result into an unknown one — so it is not merged. Measured: a batch of 3 calls costs 4 appends instead of 2N = 6 (one for the intents plus one completion per call), and a test pins that number. A single append measures about **1.0 ms** with the index reused (see the large-log baseline), so a three-call batch takes that cost from roughly 6 ms to roughly 4 ms. **The reason completions are not merged is correctness, not that saving**: it buys noise-level time and pays for it by deferring the record of an operation that already happened past the point where the process can die.
- **A failed record no longer leaves a permanent unknown.** When the completion append fails, the result is kept in memory and queued for compensation; the next append that succeeds (the sync at the end of the request, for instance) writes the completion for the same call ID. The event ID is derived from the call ID, so the compensation is an idempotent retry rather than a second record, and a reload reads the definite outcome instead of "it may have happened".
- **A recovery conclusion must state its evidence**, in three states: the target's current content **matches** the hash recorded before the call → judged **not to have happened** (sufficient evidence); it **differs** → `outcome_unknown`, reported with both hashes and the statement that this call is not proven to be the cause; the target **cannot be read**, or **no hash was recorded** → `outcome_unknown`, reported as an absence of evidence rather than as proof of innocence. Nothing is ever replayed.
- **A large target gets weak evidence only.** Above 4 MiB `write_file` no longer reads and hashes the target to describe its intent, because that would charge a full file read to every side effect; it records a `weak:size=…:mtime=…` marker instead. That marker is weak on purpose: a rewrite that preserves size and timestamp leaves the same value, so recovery treats it as a hint, says so in the diagnostic, and never draws a conclusion from it.
- **Per-turn persistence.** A model turn and a tool turn are written as soon as they are committed, instead of waiting for the sync at the end of the request. Killing a run in the middle therefore loses at most the turn that was still in flight, and never leaves a file that was written by a turn the conversation does not mention.
- **The write order is synchronous serialization, not a queue.** Within one process every append happens in call order and `Store.Append`'s mutex fixes the log order; the loop continues only after the append returned, so the "intent precedes the side effect" guarantee is never weakened by a deferred write.
- A turn event's identity is the turn ID, which is a host-owned UUID, so the incremental write of a turn and a later sync replay of that turn are literally the same event ID: the log recognizes the second one as a retry rather than writing a second copy.
- A failing commit hook is a persistence fault: the in-memory results are kept, no new side effect starts, the run report records `stop_reason: persistence_failed`, and the next successful sync fills the log in.

### Resource conflict keys

Conflict detection uses one canonical resource key, and Bash, the file tools, the directory and search tools and the resource coordinator all build their claims through the same entry point:

- A key is cleaned, separator-normalized to `/`, and stripped of a trailing separator (a volume root keeps its separator).
- Windows applies a conservative case normalization: it may serialize two names that Windows would treat as distinct, but it never misses a conflict, and a missed conflict is the dangerous direction. POSIX never lowercases unconditionally.
- A read-only Bash command's whole-workspace tree claim and a write inside that tree always conflict.
- When a resource identity cannot be established (empty path, unknown resource kind, unknown access mode) the call falls back to exclusive execution.
- Known boundaries: hard links and other aliases that reach one file through different paths, a UNC path versus the mapped drive letter, and per-directory case sensitivity on Windows are not recognized; those cases degrade to serial execution.

### Web fan-out and execution identity

A batched web query expands one model call into several concurrent executions. Three identities are kept apart:

- The model call ID is supplied by the model and is only used to pair a result with the provider protocol; the collapsed tool result still carries the parent call ID.
- The execution ID is allocated by the host, is never built from a model-controllable ID, and skips every model call ID already used in the turn. A model that returns both `a` and `a:1` therefore cannot make the fan-out of `a` collide with `a:1`.
- The parent call ID is recorded explicitly on the tool call and in the audit record. Events, audit entries and child results use the execution ID and carry the parent relation; no code parses a string prefix to infer it.
- A web activity ID is derived from the host execution identity, so the start and completion projections agree and the UI does not show a duplicate.
- Result order still follows the original model call order.

### Stop reasons and the request budget

Stop reasons have an explicit handling table, and a non-`tool_use` stop is not a successful completion:

| Stop | Behavior |
|---|---|
| `completed` | Normal completion |
| `tool_use` | The calls are validated and executed |
| `length` | The partial response is kept and not presented as complete; its calls are not executed and are closed with `not_executed` |
| `cancelled` | Usable results are kept and the request ends |
| `error` | An explicit failure is returned |
| Unknown value | Protocol error; the response is not written to the transcript |
| Turn limit reached | A recoverable history is kept and the caller is told the request did not complete normally |
| Budget exhausted | No further model request is started, and committed calls are closed |

- A length truncation is not continued automatically. When truncation leaves tool arguments incomplete, that call is not treated as executable: the arguments are recorded as an empty object and the call is closed with `not_executed`, an unpairable call is dropped, and the partial answer is kept.
- The Responses adapter reads the envelope `status` and `incomplete_details`: `incomplete` maps to `length` (whether the cause was the token limit or a content filter), `failed` and `cancelled` map to `error` and `cancelled`, and an unrecognized status is never treated as a completion — it is read as `length`, so the partial answer is kept, its calls do not run, and the response is not presented as finished.
- The stop-reason mapping has exactly one implementation (`StopKindFor` in `internal/driver`). An adapter only translates its own dialect into the shared vocabulary; whether a response is refused and whether its calls run is decided by the same policy table:

  | Provider behaviour | Default | Relaxable |
  |---|---|---|
  | `tool_calls` / `function_call` (asks for tools) | `tool_use`; the calls are validated and executed | no |
  | `length` / `content_filter` / `max_tokens` and other truncations | `length`; the partial response is kept and its calls are closed with `not_executed` | no |
  | `completed` without calls | `completed` | no |
  | `completed` **with** calls | Protocol error; the response is not written to the transcript | yes, see below |
  | `failed` / `cancelled` | `error` / `cancelled` | no |
  | Unrecognized value | Protocol error; the response is not written to the transcript | no |

- **Interoperability relaxation (risky).** Some gateways report `finish_reason: "stop"` while returning tool calls. That response is refused by default, because "finished" and "there are still calls to run" contradict each other: committing it would either strand the calls or claim a completion that never happened. If a provider really behaves that way, it can be trusted explicitly, per provider:

  ```toml
  [providers.gateway]
  adapter = "openai_chat"
  base_url = "https://gateway.example/v1"
  model = "some-model"
  accept_tool_calls_with_stop = true
  ```

  With it on, that response is executed as `tool_use` and recorded as `accept_tool_calls_with_stop` on the response, in the run report (`interop`) and in the audit trail. It is configured per provider and covers only this one row: a failed response, a cancelled one and an unrecognized value are never relaxed. Turning it on makes a response that used to "look successful" actually run its calls, so enable it only after confirming what the gateway means.
- Text output states a truncation or cancellation on stderr, while `json`, `jsonl` and the TUI expose the structured `stop` field.
- The request-level budget covers the main model calls, the context-compaction summaries and the context-recovery retries. Every model call passes a pre-request admission check against the estimated input plus the output reserve, and the provider's real usage calibrates the totals afterwards; a missing usage marks the totals as an estimated lower bound.
- **Admission semantics.** The estimated input plus the output reserve has to fit in what is left of the budget, otherwise that call is not started at all: the request is refused before the model is called. Setting `max_total_tokens` below the estimated prompt therefore fails the request outright instead of spending one call first. How to tell: the run summary records `stop_reason: token_budget`, the error is `agent token budget exhausted before the next model call` and carries the limit, and stderr shows it. With `max_total_tokens = 1000` and an estimated prompt of 1200, the model is never called.
- Reasoning tokens are recorded separately and never counted twice, because every adapter already includes them in its output tokens. The response returned by `Run` still describes only the last call; the accumulated usage is exposed separately through `LoopOptions.Usage` (`RunUsage`).
- **Two usage figures, named apart so neither impersonates the other.** `response.usage` (the existing top-level `usage` in `json`) describes the **last model call**, which is what a single diagnostic needs; the accumulated figure describes the **whole request**, comes from `RunUsage`, and is what cost accounting needs. `LoopOptions.Usage` now has a production call site - it had none, so the accumulated number was never collected at all - and:
  - `json` gains top-level `request_usage` and `request_model_calls` **without renaming anything**: the response is embedded, so `turn`, `stop` and `usage` keep their names and meanings;
  - `jsonl` gains a separate `{"type":"request_usage",...}` line and leaves the `response` line unchanged;
  - metrics record both and the summary accumulates them separately, and `/run` shows the request totals too.
- **Cache accounting.**
- **Cache accounting.** `cached_input_tokens` is a **subset** of `input_tokens`, never an addition: every provider counts a cached prompt token in its input tokens as well, so the figure exists to tell a cache hit from a miss for cost accounting and does not change what the budget is charged. A provider that reports no cache breakdown leaves it at 0, which does not make the usage inexact. Both `RunUsage` and the run summary expose it.
- The current drivers do not forward a remaining output limit to the provider, so the budget is soft: Eylu stops starting new requests once the limit is reached, but a single call may still overshoot. A subagent keeps running after the parent request ends, so its cost is reported separately instead of being folded into the same synchronous budget.

### Tightening safety settings while a request runs

- Narrowing a safety setting takes effect **inside the current request**, not on the next one: narrowing the mode (`full → auto`, `auto → plan`, `plan → manual`), adding a `deny_tools` entry, disabling an MCP server, or moving the hosted-web permission from `allow`/`ask` to `deny` makes the host stop the running request before its next batch boundary.
- Stopping is not rolling back: a side effect that already happened is not undone and a result that was already obtained is kept; calls that had not started are closed with `not_executed`, and a call waiting for approval does not start even once the approval arrives.
- The run report records `stop_reason: policy_tightened` with the reason, and the TUI and CLI say the request stopped on the new settings instead of presenting it as a request failure or a model fault.
- Widening only applies to the next request; the running one finishes normally, because a request is never granted a permission it did not have when it started.
- The decision has one implementation (`Tightening` in `internal/policy`), so every entry point answers "did this narrow?" the same way, and an unrecognized hosted-web permission can only ever stop a request, never widen one.

### Event IDs and append idempotency

- Every logical event has a stable ID. When an append result is unknown, the retry reuses the ID; the store recognizes an ID it already holds with identical content as a retry and does not write a second copy, while the same ID with different content is reported as a conflict instead of being overwritten.
- A state event is only appended when its payload changes, and two identical states (A→B→A) get fresh IDs rather than being mistaken for a retry.
- The same prompt text submitted twice is a legitimate repeat: the event identity includes its position, so identical text is never deduplicated.
- When a write result is uncertain (Write/Sync/Close reported an error), the store drops its cached tail and event index, and the next append re-reads the log before continuing the sequence, so the in-memory sequence never drifts from the file.
- Each process uses its own event ID prefix, so a restart cannot collide with IDs an earlier process wrote.
- Reader compatibility: a log written before IDs existed still loads (its events derive an identity from their sequence); a repeated ID whose content is identical is not applied twice, and a conflicting one is reported as a diagnostic instead of being silently rewritten. A document from another schema version is still refused explicitly rather than misread.
- **Conservative duplicate-turn diagnostics.** A log written before event IDs existed can hold the same turn ID under two different sequences, which used to make the session unopenable with an opaque `session contains duplicate turn ID`. The load now splits on whether the content matches: identical content applies one copy and produces a **resolved** (`benign`) diagnostic, conflicting content keeps the first copy and produces a diagnostic that asks for a human. Neither case rewrites the raw log, because the log is evidence and guessing which copy is right would destroy it.
- Diagnostics are graded when deciding whether `--resume` may proceed: a `benign` diagnostic - a repeated event or turn whose content was identical, where the loader lost nothing - no longer blocks a resume and is only stated on stderr; every other diagnostic still refuses the resume with actionable information. The duplicate-turn message now names the session ID, the turn ID and the way out (load with `LoadRecovering` to have repeats merged, or inspect the session event log).

### Conversation concurrency, lock boundaries and callbacks

A Conversation has exactly one writer:

- Run mutual exclusion is separate from the state lock: run ownership covers a whole request, while the state lock is only held for brief reads and commits.
- A second concurrent request on the same conversation returns `ErrConversationBusy` immediately instead of blocking indefinitely, and ownership is released as soon as the request finishes or is cancelled.
- The model call, the tool batch, the approval callback, `BeforeModel`, emitted events, `ContextEvent`, persistence and any other host-supplied function all run outside the state lock. A callback can therefore call `ContextReport` or `ExportState` without deadlocking, and committed state stays readable while a model call is blocked.
- Context events are buffered by the context layer and delivered to the host after the state lock is released.
- An immutable request snapshot (turns, tools, driver state) is taken at the start of each round, so a model call never reads half-updated state.
- A provider change that arrives during a request is queued and applied at the next round boundary, and `CancelRun` lets a host stop the current request immediately when it tightens a safety setting, so no further tool call starts.
- Rotating to a new session waits for the running request (cancelling it first if it overruns the grace period), so it can never interleave with a request that is still committing state.
- The lock order is always run ownership then state lock, and the session persistence lock never forms a reverse dependency with the state lock.

### Execution checkpoints and crash recovery

A side-effecting tool persists its execution intent before it starts, which shrinks the window in which an effect has happened but nothing records it:

- An intent that cannot be written means the call never starts and reports `not_executed`: only an operation the host can log is allowed to happen.
- The terminal outcome is persisted as soon as the operation is over. When that record cannot be written, the in-memory result is kept, the batch stops starting new side effects, and the failure is reported explicitly as "the operation may have completed but its record did not" (the result metadata carries `checkpoint_incomplete`).
- Read-only calls need no intent, and streaming text deltas are never written per token.
- The file tools provide verifiable hints: `previous_hash` (the content hash before the change) and `file_hash` (the hash after it), plus the target path in the audit record. Those hints only help a human decide whether the change happened; they are never used to replay it.
- Recovery rules: an intent without a completion means the call is `outcome_unknown` and is never re-run; a recorded completion is used directly; shell commands and network requests are non-idempotent and need explicit confirmation by default.
- The session schema is now 3, adding the lifecycle events and the stable event ID. A version 2 session is still readable, an older version is refused explicitly on load, and `Migrate` upgrades one while keeping a `.v<old-version>.bak` backup.

### Run observability

Every finished request writes a summary into the session log (a `run_reported` event, `last_run` in the snapshot), so "why did it stop, what ran, what is unknown" can be answered from the log rather than only from a transient UI event:

- Stop reason: the model's stop reason, or `iteration_limit`, `token_budget`, `cancelled`, `abort_request`, `event_sink_failed`.
- Counters: model calls, tool executions, and the separate counts of `succeeded`, `failed`, `rejected`, `cancelled`, `not_executed` and `outcome_unknown`.
- Usage: input, output, reasoning and cached-input tokens plus whether they are exact; also exposed to the host through `LoopOptions.Report`.
- Recovery diagnostics: the call IDs this request closed with `outcome_unknown`.

The summary never contains a plaintext key or a sensitive request header, and tool arguments and content still follow the existing redaction, truncation and externalization rules.

### Event delivery

Delivery has two classes with different contracts:

- **Critical events** (a tool starting or finishing, an approval result, usage, the terminal report — anything carrying control or durable state) are delivered synchronously and in order. They are never dropped by buffering; a delivery failure stops the request immediately and is recorded as `event_sink_failed` in the run summary.
- **Streamed deltas** (text and reasoning) are progress rather than state: their content is the concatenation of the pieces and the transcript holds the full answer regardless. They are coalesced within a bounded 4 KiB buffer, so a long answer neither blocks the model nor appears only at the end.

A slow consumer has an explicit meaning:

- One delivery that overruns a 250 ms budget marks the host as behind. From then on streamed deltas are not pushed to it at all: they are dropped and counted in the run summary as `events_dropped`. Critical events keep being delivered synchronously and in order. A delivery that completes in time marks the host as caught up and delivery resumes.
- A slow consumer therefore cannot hold the model stream open forever. The price is that the host's view of the streamed text may be incomplete, and that price is **observable**: `events_dropped` and `warnings` are both recorded in the run summary, while the transcript stays complete.

The audit callback contract is equally explicit:

- A host audit sink that fails or panics is isolated: it changes neither the call state nor the result, is never retried, never blocks, and never ends the request. The reason is direct — letting a host callback end a request that already committed a side effect would lose the result of work that really happened.
- The failure is counted in the run summary as `audit_failures`, and the first one is stated once on stderr as an `[audit]` diagnostic.
- "The request succeeded" and "the audit trail is incomplete" can both be true and both be visible: `warnings` names how many records could not be written and the reason for the most recent one.

- **The six-event lifecycle table** (`docs/Eylu Agent Loop 改进计划.md` §13.3) maps onto the events that are actually recorded:

  | §13.3 event | Recorded as |
  |---|---|
  | `request_started` | `request_started` (new), written before the first model call |
  | `model_turn_committed` | `turn_appended`: the per-turn write from PR-21 *is* the durable record of a committed model turn, and a second event carrying the same turn would only duplicate it |
  | `tool_prepared` | `tool_prepared` (new), sourced from the pending set and carrying the call, the request and the round |
  | `tool_execution_intent` | `tool_execution_intent` (existing), written before the side effect |
  | `tool_completed` | `tool_completed` (existing), with the compensation path behind it |
  | `request_finished` | `run_reported` (existing), carrying the run summary |

  The two new events are **evidence, not state**: they do not rewrite the snapshot and they change no recovery conclusion (a committed call's outcome is still decided by its intent and its completion). They exist so the log can answer the question the intent alone cannot - a call that was prepared and never got an intent is **provably not executed**. Both carry an optional `request_id`; a log written before the field existed simply has none and reads the same.

### One conclusion from four outputs

- The same request reaches the **same conclusion** in `--output text`, `json`, `jsonl` and the TUI history. The wording has one source (`agent.RunStopNote`): `length`, `cancelled`, `error`, `token_budget`, `iteration_limit`, `policy_tightened`, `persistence_failed` and `event_sink_failed` all come from it, so the CLI and the TUI cannot describe one request differently.
- **The TUI no longer shows half an answer with no explanation**: a request that did not finish writes one line into the history, in the same sentence the CLI uses, and the timing line says `Stopped after` rather than `Completed in` - "Completed in 1ms" above a truncated answer is the same defect as no note at all.
- When the failure's own sentence is the note, only one line is written instead of two.
- `/run` shows the summary of the most recent request (`stop_reason`, model and tool call counts, the terminal-state counts, tokens, cache hits, recovery diagnostics, warnings). It renders the run report itself rather than the transcript, so the interface and the log cannot diverge. `/run` is in the TUI completion list.

### Core loop responsibilities

The Web-specific logic has moved out of `loop.go` into same-package collaborators: `web_runtime.go` (plan resolution and MCP refresh), `web_calls.go` (batch expansion and parent mapping), `web_results.go` (content, activity and citation aggregation), `tool_events.go` (event projection), `run_finalize.go` (terminal states, pending closure and stop reasons), and `event_queue.go` (event delivery). The core loop only obtains the current run snapshot, prepares the context, calls and validates the model, commits the response, executes the tool batch, commits the results, and decides whether to continue. Its control flow depends on neither Web metadata nor event-delivery details.

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

### The measured benefit of deduplicating a repeated read

`internal/tool/read_dedup_benefit_test.go` measures the token benefit of a repeated read per file shape, using **real reads** and a 1 byte/token estimator:

| Shape | Body | First read | Repeated read | Benefit | Deduplicated |
|---|---|---|---|---|---|
| empty file | 0 B | ~0 | ~0 | 0 | no (nothing to replace) |
| single line | 14 B | ~14 | ~173 | **-159** | yes |
| no trailing newline | 29 B | ~29 | ~173 | **-144** | yes |
| CRLF and LF mixed | 21 B | ~21 | ~173 | **-152** | yes |
| Unicode multibyte | 67 B | ~67 | ~173 | **-106** | yes |
| one very long line | 40,001 B | ~40,001 | ~173 | **+39,828** | yes |

- **Deduplicating a small body is a net loss - now fixed.** The reference is roughly a fixed size (~130-200 bytes), so when the body is smaller than the reference, replacing it **grows the request**. A body is now only replaced when the reference is genuinely smaller; otherwise it keeps its body. The comparison is by **bytes**, not by estimated tokens: an estimator can round a tiny body to zero and would then "save" it into something larger, and bytes are the conservative direction for multi-byte content, which costs fewer tokens per byte. Skipped replacements are counted as `NotWorthReplacing` and recorded on the block as `deduplication_not_worth_it` metadata - an auditable decision rather than a silent absence.
  - This changed an existing contract: several PR-05 cases asserted "a repeat deduplicates" with 10-30 byte bodies. They are about the **rule**, not the fixture size, so their fixtures now use a body comfortably larger than a reference (`bodyLargerThanAReference`) - and the cases asserting a body is **kept** were enlarged too, because with a tiny fixture they would pass for the wrong reason and quietly stop discriminating.
- **A11 holds, and the first measurement was wrong.** The first version ran only "read tool -> prompt builder" and **skipped the trimming step in `contextualizeTurn`**, so it measured an untrimmed body and concluded, incorrectly, that the premise did not reproduce. With the trimming included it does: a 4000-byte single line under a 512-byte budget is cut **inside the line** (the `headLines == 0 && tailLines == 0` branch of `trimToolResultContent`, whose own comment says this is what a very long single line looks like) and returns `nil` line ranges; because `len(retained) == 0`, `contextualizeTurn` writes `context_truncated` and no `retained_ranges`. That fragment is neither a complete slice nor a fragment canonical, so **a repeated read keeps paying 512 tokens every time**. See `internal/agent/long_line_dedup_test.go`.
- **Implemented (§8.3 item 1, in-line byte intervals).** When a body is cut inside a line, the byte spans that survived are now reported (`retained_bytes`, `{line, start, end}`, relative to the line start, end exclusive), and the coverage judgement extends from "line ranges only" to "line ranges **or** in-line byte intervals". Measured in `internal/agent/long_line_dedup_test.go`: a 4000-byte single line under a 512-byte budget goes from **512 tokens to 198 tokens** for the repeated read, a saving of 314.
- **The rules that protect red line 1**, each with a test in `internal/context/byte_spans_test.go`:
  1. An in-line byte span is **never** read as a line range. A canonical holding part of a line **does not cover** a whole-line read of it - otherwise a full body would be replaced by a reference to a body that does not contain it.
  2. A reference is used only when the current slice is **itself** a fragment of that same line and the bytes it holds are **fully contained** in the canonical's spans.
  3. A fragment of a line **never** supersedes another canonical (it holds no whole line, so it is not the better authority for any larger range) and never rewrites an existing reference.
  4. The reference text says it stands for **part of a line** (`bytes=1:0-300,1:900-1000`), so the model is not misled into thinking it has the line.
  5. Different file hashes are different bytes, as before.
- Every other case - a wider fragment of the same line, a fragment of another line, a fragment of another revision, no spans at all - **keeps the body**. The negative cases and the positive ones together are the acceptance criterion.
- Body correctness is unaffected either way: a shape that deduplicates must still announce that it is a reference (the test asserts it), and every PR-05/PR-14 regression case is kept.

### What a large log costs to load and recover

The first `Append` builds the event index used for idempotency, and it pays for that by reading the whole log once. Measured on this host (Windows) with the benchmarks in `internal/session/perf_test.go`:

| Events | First `Append` (index build) | Later `Append` (index reused) | `Load` | `Load` allocation |
|---|---|---|---|---|
| 10^4 | 90.6 ms | 1.0 ms | 55.0 ms | 36.9 MB / 220k allocs |
| 10^5 | 895 ms | - | 511 ms | 393 MB / 2.2M allocs |

- **The curve is linear**: ten times the events costs about ten times the time and memory (first append x9.9, load x9.3, allocations x10.0), with no quadratic behaviour. That is why PR-27's third item - incremental or windowed indexing - is **not needed**: it exists for a non-linear cost, and there is none here. Adding it would introduce a second piece of state that would have to be argued correct.
- The index is built once: within one process the next append drops from 90.6 ms to 1.0 ms, about 87x.
- There are two thresholds, both deliberately loose, because they exist to catch a change in complexity rather than a slow machine: a five-second budget for the first append and for the load, and a tighter bound on the load's **allocation count** of 60 per event (measured: 22.0 per event). Allocation counts are deterministic, which makes that guard firmer than the timing one.
- Loading 10^5 events allocates about 393 MB transiently, which is the cost of decoding JSON line by line and is reclaimable; it is recorded because it is the one figure worth noticing at that scale.
- A large log changes the cost, never the conclusion: the test requires the load and the recovery to agree on the session, the sequence and the number of prompts, and the existing idempotency and recovery tests pass unmodified.

### Soak and stress tests

- `internal/tool/soak_test.go` searches for concurrency defects over many rounds: parallel batches on conflicting resources, repeated cancellation, and **a host-callback fault injected into every round** (the audit sink panics on every call, the checkpoint fails every third intent). Six rounds by default; `EYLU_SOAK_ROUNDS=<n>` lengthens it locally.
- Every round asserts: each call reached a terminal state, the coordinator holds no waiter or grant, no goroutine was left behind (a `runtime.NumGoroutine` baseline regression), and each call appears in the log exactly once as an intent and once as a completion.
- **The leak detector has its own test**: `TestSoakLeakDetectorNoticesAnUnfinishedGoroutine` deliberately leaves a goroutine that never finishes and requires the detector to report growth - otherwise "no leak" could just mean the detector is not working.
- Known limit, written into the test as well: the "two calls on one path must not overlap" search **has not been shown to fire**. Disabling the scheduler's `canStartCall`, the coordinator's conflict predicate, and both together all left the two same-path calls serialized, so the mechanism producing that has not been identified. The witness is proven to be exercised, so the search is real rather than vacuous - but a pass must not be read as proof that the serialization path was tested.

### Tests for the platform-specific branches

- The `//go:build !windows` and `//go:build unix` branches (atomic replace, directory fsync, process-group cancellation) **never execute on a Windows host**, so their tests carry the same build tags: `internal/session/replace_other_test.go` and `internal/tool/process_tree_unix_test.go`. They really run on the `ubuntu-latest` and `macos-latest` CI legs, and that is where the evidence for those branches comes from.
- The strongest local evidence a Windows host can produce is **cross-linking**: `GOOS=linux|darwin go vet ./...` and `GOOS=linux|darwin go test -c ./internal/session/ ./internal/tool/` both pass (the test binaries build and link), but nothing is executed. Do not read "it compiles" as "it was verified".
- The symlink behaviour (including `EvalSymlinks` resolution and the out-of-workspace refusal) already has a test that skips on Windows when creating a symlink is not permitted and really runs on POSIX; the path key's case policy is parameterised through `resourceKeyFor(goos, path)`, so both policies are asserted on any platform.
- The local way to exercise the Unix script path on Windows is Git Bash: `scripts/verify.sh` and `scripts/smoke.sh` run through `%ProgramFiles%\Git\bin\bash.exe`.

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
