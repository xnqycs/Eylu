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

`--resume <session-id>` loads an existing session from the current workspace exactly. An invalid, missing, damaged, or cross-workspace ID returns a non-zero exit code and leaves session storage unchanged. Interactive text sessions print a directly executable resume command when they exit.

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
startup_timeout_seconds = 15
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

An `sse` server uses the same `url`, header, and OAuth fields. Static headers can be set with `headers`; inject sensitive values through `environment_headers`, `bearer_token_environment`, or OAuth. The compatibility fields `disabled`, `timeout_seconds`, and `read_only_tools` remain supported. Startup, call, and OAuth/interaction timeouts default to 15, 60, and 30 seconds.

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

Enter `/mcp` in the TUI to open the server list and detail panel. It exposes tools, resources, prompts, connection state, diagnostics, reconnect, enable/disable, and login actions. Catalog notifications atomically refresh the tool registry, context, and cache fingerprint. OAuth credentials are stored in `~/.eylu/mcp_credentials.json` with file locking, atomic replacement, and platform-specific permission hardening.

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
