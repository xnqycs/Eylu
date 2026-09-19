# Eylu architecture and reliability

This document states how Eylu is put together and which guarantees the current
code makes. It is written so that a reader can tell three things apart:

- **planned** — agreed but not implemented;
- **implemented** — in the tree, with a named test;
- **verified** — actually exercised, and on which platform.

A passing test suite is not a statement that no defect remains. Where a guarantee
was only exercised on one platform, or only by static reasoning, this document says
so.

---

## 1. One-page architecture

```
                      ┌──────────────────────── entry points ────────────────────────┐
                      │                                                              │
              cli (app.Execute)                                        tui (tuiBackend)
                      │                                                              │
                      └───────────────► toolRun (request_runner.go) ◄────────────────┘
                                        builds the executor, binds the
                                        checkpoint, prepares the loop
                                        options, settles the outcome
                                                       │
                                                       ▼
                                              agent.Conversation.Run
                                                       │
        ┌──────────────────────────────────────────────┼──────────────────────────────┐
        │                                              │                              │
        ▼                                              ▼                              ▼
 contextledger.PromptBuilder                   tool.Executor                 driver.ModelDriver
 (projection + budget ledger)                  (policy, approval,            (one dialect each)
                                               scheduling, checkpoint)
        │                                              │                              │
        └───────────────────────┬──────────────────────┘                              │
                                ▼                                                     │
                        session.Store  ◄───────────────────────────────────────────  ┘
                    (events.jsonl + snapshot.json + attachments)
```

The layers only point downwards. `agent` does not know about `ui`, and no layer
knows about an entry point: the difference between the command line and the
interface lives in which callbacks they pass, not in which guarantees they get.

## 2. Request lifecycle and ownership

One request is one *turn loop* owned by exactly one writer.

```
   toolRun.executor ──► executor.Checkpoint = session      (a side effect always has a sink)
          │
          ▼
   toolRun.prepare ───► RecordRequestStarted               (the request is bracketed in the log)
          │             OnTurnCommitted, OnToolPrepared     (turn-by-turn durability)
          ▼
   ┌────────────────────────── runFinalizer owns every exit ──────────────────────────┐
   │                                                                                  │
   │  iteration:                                                                      │
   │    prepareRequestContext ──► deterministic build (lock)                          │
   │                          ──► compaction summary (NO lock, model call)            │
   │                          ──► commit or reject the summary (lock)                 │
   │    generate ─────────────► admission check ──► model call (NO lock)              │
   │    normalize ────────────► refuse a malformed response ──► finish(aborted)       │
   │    commitResponse + trackCommittedCalls (lock)                                   │
   │    commitTurn ───────────► host persistence; failure ends the request            │
   │    ExecuteBatch ─────────► tools; the executor owns the request-level control    │
   │    commit the tool turn ─► publish, sync                                          │
   └──────────────────────────────────────────────────────────────────────────────────┘
          │
          ▼
   toolRun.settle ────► RecordRunReport, then Sync           (the reason survives a failed snapshot)
```

Ownership is one object, `runGate`, and it is used by everything that changes the
conversation as a whole:

| operation | ownership |
|---|---|
| `Run`, `Send` | `begin` — refuses a second writer with `ErrConversationBusy` |
| `NewSessionWithEnvironment` | `acquire` — waits for the current owner, then holds the slot until done |
| `Compact` | `begin` — refuses to interleave with a running request |

`begin` never blocks and `acquire` waits; both take the slot under the same lock, so
"the previous request finished" and "the new owner holds the conversation" are one
step. The state mutex (`Conversation.mu`) is held only for short reads and commits:
no model call, no host callback and no compaction summary runs inside it.

## 3. Tool execution phases and checkpoint order

```
 prepare ─────────► policy decision ─────────► approval ─────────► concurrency claim
 (no side effect)                                                       │
                                                                        ▼
                                              ┌── intent persisted ──────────────┐
                                              │   (batch write when safe,        │
                                              │    per call otherwise)           │
                                              └──────────────┬──────────────────┘
                                                             ▼
                                                     the tool executes
                                                             │
                                              ┌──────────────▼──────────────────┐
                                              │   completion persisted          │
                                              └──────────────┬──────────────────┘
                                                             ▼
                                                        terminal state
```

Rules that hold at every phase:

- **Preparation modifies nothing.** Resolving a target path does not create its
  parents; the parents are created by the write, after the intent is durable. A
  cancelled request stops reading the target it will not write.
- **A failed intent prevents the call.** An operation the host cannot record does not
  happen.
- **A failed completion keeps the result.** The operation already happened: the
  in-memory result is kept, no further side effect starts, and the record says the
  outcome may have happened.
- **The evidence a call records is a hint.** It is collected before the resource
  claim is held, so a match proves nothing on its own; recovery reports a weak
  marker as weak.
- **An accepted interruption stops the batch.** Once the executor accepts a user
  interruption, no later call of that batch starts, and calls that never started are
  closed as `not_executed` with the interruption on the result.

## 4. Session log: physical watermark, logical dedup, recovery

The log is the evidence; the snapshot is a cache of it.

- **Physical sequence** — every valid line in `events.jsonl` has its own sequence,
  strictly increasing by one. It is the log's own watermark and the position the next
  append continues from.
- **Logical deduplication** — an event ID that the log already holds is not applied
  twice. Identical content is a retry of one logical event; different content is a
  conflict, which is reported and never silently rewritten.
- **Consumption** — deduplication stops the content from being applied twice. It does
  not make the physical record disappear: a skipped record still advances the
  watermark, so the records behind it do not read back as a gap.
- **Recovery** — `LoadRecovering` truncates a damaged tail, reports the diagnostics
  and rebuilds the snapshot from the valid prefix. A repeated record that was
  consumed without being applied leaves nothing pending.
- **Append position** — cached from the last valid physical record. The snapshot
  watermark may legitimately lag behind the log, so it never drives an append.

## 5. Entry points: what is shared and what may differ

Shared, because it is the reliability contract:

- the executor and its checkpoint (`toolRun.executor`);
- the request deadline (`toolRun.requestContext`);
- request start, turn persistence and the prepared-call hook (`toolRun.prepare`);
- the run summary and the snapshot sync, with the sync error leading (`toolRun.settle`);
- the request metric and its totals (`requestMetric`).

Allowed to differ, because it is presentation and interaction:

- how a submission is parsed and how references are resolved;
- how approval and questions are asked;
- how events are rendered, and the text layout of an answer;
- the identifiers and timestamps the output happens to use.

`internal/app/entry_consistency_test.go` drives both entries with the same scripted
model and compares which calls ran and how they ended, the run report's counters,
the roles of the turns the session holds, the reason the request stopped, its cost
and the lifecycle records - while ignoring text, timestamps and identifiers. Each
scenario also carries an absolute contract, because a comparison alone would be
satisfied by two entries that are wrong in the same way.

## 6. Context projection, rich content and budget

A tool result has two lifetimes. The transcript stores everything the tool returned;
a model request carries one bounded text field per result, and everything it carries
is charged to the context budget.

`protocol.ProjectToolResult` is the single projection, used by both the ledger and
the drivers:

| stored | projected |
|---|---|
| text only | the text, unchanged |
| text block that repeats the result text | dropped, the text is already there |
| image or audio | type, media type, byte count and digest, plus a note that the bytes stay with the session |
| embedded resource | uri, media type, and its text if it fits; a blob becomes bytes plus a digest |
| resource link | uri, name, media type, declared size |
| structured content within the bound | carried as it is (it is already JSON) |
| structured content over the bound | replaced whole by a valid JSON summary with a digest |
| more blocks than the bound | the first `MaxProjectedBlocks` are described, the rest are reported as omitted |

The bounds are `MaxProjectedTextBlockBytes`, `MaxProjectedStructuredBytes` and
`MaxProjectedBlocks`. The estimator counts the projection rather than the stored
result, which is what keeps "what was charged" and "what was sent" the same object.

**Trust.** The projection says what the model reads; it does not say where the text
came from. `internal/context` grades every category of context by authorship -
`TrustHost`, `TrustUser`, `TrustDerived` and `TrustExternal` - and everything graded
external arrives inside an untrusted envelope:

```
<<<untrusted-data id=0123456789abcdef>>>
...the text as the host read it...
<<<end-untrusted-data id=0123456789abcdef>>>
```

The identifier is a digest of the text between the markers, which is what makes the
envelope a pure function of the content and what makes it impossible for content to
close its own envelope: a body that ended it early would have to contain the digest
of the text containing it. Because the frame is deterministic, a repeated projection
is byte-identical - so the ledger charges the framed bytes and a driver sends those
same bytes, and the frame costs budget rather than escaping it.

The envelope is applied where the request is assembled, around the projection rather
than inside it, so the projection keeps its own contract of being valid JSON. It
covers tool results, a server's instructions and resource catalog, the skill catalog
and a skill body. It does not cover the system prompt, the user's own messages or the
host's own bookkeeping, and it does not cover tool schemas and their descriptions,
which travel as protocol fields rather than as content.

**Budget.** The budget is soft and covers the whole request: the main model calls,
the compaction summaries and the context-recovery retries. A call is counted when it
happens, whether or not the provider reported tokens; a request with any unreported
usage reports itself as an estimate rather than as exact. A compaction summary is a
paid call and is only started when the request's admission check accepts it;
otherwise the deterministic compaction is used, which costs nothing. The current
drivers do not forward a remaining output limit to the provider, so a single call may
still overshoot the limit - this is a known boundary, not a hidden one.

## 7. Application-layer permission versus an OS sandbox

The policy layer decides *whether Eylu will run* a call. It is not an operating
system sandbox:

- a command is classified by reading the line with the rules of the shell that will
  run it (`policy.Config.Shell`), so a construct that is quoted text to one shell and
  a separator to another is not mistaken for inert;
- argument validation fails closed: an option family that can delete, move, execute
  another program or write a file is refused rather than trusted;
- anything the reader cannot prove falls back to `unknown`, which requires
  confirmation instead of running automatically;
- once a command runs, its own privileges are the process's privileges. Isolation of
  an untrusted tool would need process-level sandboxing, which Eylu does not
  implement.

### Trust boundaries

The section above says where the permission layer stops. These are the boundaries
inside it that a delegated or remote actor must not be able to move. They are stated
as guarantees, not as the code that currently happens to hold them up, because each
one of them is worth more than the implementation that satisfies it today.

| boundary | guarantee |
|---|---|
| T-01 | An MCP server cannot declare its own tool read-only. A remote tool's risk is granted by the local configuration alone, and a tool that configuration does not name is a write. |
| T-02 | An MCP tool always carries the name of the server it came from, so it cannot stand in for a built-in tool or for another server's tool; a name that is already taken is refused rather than replaced. |
| T-03 | Only a tool the host registered as its own can raise a request-level control state. The words a tool returns, and the annotations a server attaches to it, are data. |
| T-04 | A subagent runs under its parent's permission mode and never a wider one, and the policy checker that decides what may run is the parent's. |
| T-05 | A subagent cannot delegate again, and cannot ask the user a question. |
| T-06 | A subagent shares its parent's resource coordinator and checkpoint: two agents still cannot write one path at a time, and a side effect the subagent commits is still recorded. |
| T-07 | A subagent can create a file but cannot overwrite one, because it cannot see the conversation that would justify replacing it. |
| T-08 | A subagent's cost is its own: every model call its own request made is charged to it, in its own session, and never folded into the request that delegated to it. |
| T-09 | Content read from outside the conversation - a file, a command's output, a tool result, a server's instructions, a skill's text - reaches the model inside an untrusted envelope, and text the host or the user wrote does not. The envelope's identifier is derived from the content it wraps, so it is deterministic and content cannot close its own envelope. |

Each of these is pinned by a test named after the guarantee rather than after the
function it happens to exercise, and those tests are listed in section 8.

## 8. Invariants and where they are tested

| invariant | test |
|---|---|
| I-01 an interruption stops new work | `internal/tool/control_test.go`: `TestExecuteBatchStopsStartingCallsAfterRuntimeInterruption`, `TestExecuteBatchKeepsInterruptionVisibleBesideAbort` |
| I-02 preparation has no side effect | `internal/tool/checkpoint_test.go`: `TestCheckpointIntentFailureLeavesNoDirectoryBehind`, `TestIntentPreparationStopsWhenTheRequestIsCancelled`, `TestIntentPreparationRefusesASymlinkEscape` |
| I-03 intent precedes the modification | `internal/tool/checkpoint_test.go`: `TestCheckpointIntentFailurePreventsTheSideEffect`, `TestCheckpointCompletionFailureKeepsTheResultAndStopsTheBatch`; `internal/app/checkpoint_test.go` |
| I-04 exactly one finalization per request | `internal/agent/run_finalize_contract_test.go`, `internal/agent/run_report_test.go` |
| I-05 one effective writer per session | `internal/agent/ownership_test.go`, `internal/agent/lock_test.go` |
| I-06 the state lock only guards memory | `internal/agent/ownership_test.go`: `TestStateReadsAndAStopRequestDoNotWaitForTheCompactionSummary`, `TestManualCompactionDeliversCallbacksOutsideTheStateLock` |
| I-07 physical position and logical dedup are separate | `internal/session/store_watermark_test.go` |
| I-08 what the model sees matches the budget | `internal/protocol/projection_test.go`, `internal/context/projection_test.go`, `internal/driver/contract_test.go`: `TestDriversSendTheProjectedToolResult` |
| I-09 front ends differ only in presentation | `internal/app/entry_consistency_test.go` |
| I-10 an unknown outcome is never replayed | `internal/session/lifecycle_test.go`, `internal/app/checkpoint_test.go`: `TestResumeReportsAnUnrecordedExecutionAsUnknown` |
| the classifier reads a line as the shell does | `internal/tool/bash_shell_semantics_test.go` |
| every system segment reaches the provider | `internal/driver/contract_test.go`: `TestDriversKeepEverySystemSegmentInOrder`, `internal/driver/webnative/driver_test.go`: `TestAnthropicKeepsEverySystemSegment` |
| T-01 a server cannot declare its own tool read-only | `internal/mcpclient/trust_boundary_test.go`: `TestMCPServerCannotDeclareItsOwnToolReadOnly` |
| T-02 an MCP tool name cannot collide with a built-in tool | `internal/mcpclient/trust_boundary_test.go`: `TestMCPToolNamesCannotCollideWithBuiltinTools` |
| T-03 only a host-registered tool raises request-level control | `internal/tool/control_test.go`: `TestOnlyAHostRegisteredToolCanRaiseRequestLevelControl`, `TestAskDismissalRaisesTypedInterruption` |
| T-04 a subagent cannot run under a wider mode than its parent | `internal/agent/profile_test.go`: `TestSubagentPermissionCannotBeWiderThanTheParent` |
| T-05 a subagent cannot delegate or ask the user | `internal/agent/profile_test.go`: `TestSubagentCannotSpawnAnotherSubagentOrAskTheUser` |
| T-06 a subagent shares the parent coordinator and checkpoint | `internal/app/trust_boundary_test.go`: `TestSubagentSharesTheParentCoordinatorAndCheckpoint` |
| T-07 a subagent cannot overwrite an existing file | `internal/app/trust_boundary_test.go`: `TestSubagentWriteFileCannotOverwriteAnExistingFile` |
| T-08 a subagent's cost is not folded into the parent request | `internal/app/trust_boundary_test.go`: `TestSubagentUsageIsNotFoldedIntoTheParentRequest`, `internal/app/subagent_usage_test.go`: `TestASubagentIsChargedForEveryModelCallItMade` |
| T-09 untrusted content enters the request framed, and content cannot close its own envelope | `internal/context/untrusted_frame_test.go`: `TestAToolResultEntersTheRequestInsideTheUntrustedEnvelope`, `TestAToolResultCannotCloseItsOwnEnvelope`; `internal/agent/untrusted_context_test.go`: `TestContentReadFromOutsideTheConversationEntersFramed`; `internal/protocol/untrusted_test.go`: `TestContentCannotCloseItsOwnEnvelope`, `TestAForgedEnvelopeIsNotAccepted` |

The table is not prose on its own. `internal/docscheck` reads it and fails the build
when a row names a test file or a test function that does not exist
(`TestInvariantTableTestsExist`), so a rename or a deletion cannot leave this document
claiming a guarantee that nothing holds up any more.

## 9. Known limitations and unverified scenarios

- **Platforms.** The three-platform CI matrix is the only native cross-platform run.
  Locally, only Windows was exercised; the Unix-only branches of the process tree,
  the POSIX shell probes and the `-race` gate are covered by CI, not by a local run.
- **Windows shell dialects.** The comparison between the classifier and the shell is
  verified against `cmd.exe` and git-bash on Windows. A POSIX shell on Unix is probed
  by the same test where one exists, but the Unix leg has not been run here.
- **PowerShell.** `EYLU_SHELL` can point at any executable; the classifier only
  models the POSIX and command-interpreter dialects. A PowerShell shell is read with
  POSIX rules, which is a gap, not a proof of safety.
- **Real providers.** All driver verification is offline, against fixed stubs. There
  is no live provider round-trip in the test suite, and none is run automatically.
- **Multimodal input.** Binary content is described rather than sent. A provider that
  could accept an image receives no image, by design; a driver-side multimodal path
  is planned, not implemented.
- **Project map.** The project-map scan still runs while the state lock is held. It
  is I/O, it is bounded, and moving it out is planned rather than done.
- **Gemini.** `gemini_interactions` sends system turns as `role: "system"` input
  items. That they arrive intact is verified; whether the provider accepts that role
  is not.
- **The soft budget.** A single model call can still overshoot the request budget,
  because no driver forwards a remaining output limit.
- **Prompt injection.** Content read from outside the conversation is delivered inside
  the untrusted envelope described in section 6, and the system prompt states what the
  envelope means. That is a statement about provenance, not immunity: a model can
  still choose to follow text that is framed as data. Eylu does not scan content for
  injection patterns, because a pattern match would report a confidence it does not
  have and would flag ordinary technical documents. The envelope does not cover tool
  schemas and their descriptions, which travel as protocol fields; a tool description
  is a claim by whoever supplied the tool.
- **Recovery is advice.** A pending intent's evidence decides what a human should
  conclude; Eylu never replays an operation, and it cannot prove that a file was not
  changed by something else in between.
- **Subagent call IDs.** Lifecycle records are scoped to the request, so a parent and
  a subagent may share a call ID. The pending-intent list still matches on the call ID
  when either side has no request ID, which is what keeps older logs working.
