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
- **Recovery is advice.** A pending intent's evidence decides what a human should
  conclude; Eylu never replays an operation, and it cannot prove that a file was not
  changed by something else in between.
- **Subagent call IDs.** Lifecycle records are scoped to the request, so a parent and
  a subagent may share a call ID. The pending-intent list still matches on the call ID
  when either side has no request ID, which is what keeps older logs working.
