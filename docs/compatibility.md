# What Eylu 1.0 promises, and what it does not

This document is the compatibility contract for Eylu 1.0 and later. It exists
because 1.0 is a commitment that cannot be withdrawn: once a format is shipped,
changing it breaks somebody's parser or somebody's saved session, and the only
honest thing to do is say in advance which formats are covered by that commitment
and which are not.

The rule throughout is: **a field may be added, but never renamed, removed, or
given a different meaning within the same major version.** Every added field is
optional, so a reader written against an older shape keeps working; a reader must
in turn tolerate fields it does not know.

## Promised

### Session logs

A session written by any 1.x build is readable by any later 1.x build. The
document carries `schema_version` (`internal/session/model.go`), the store keeps a
`.bak` copy of the previous snapshot before it rewrites one, and a document
outside the supported range is refused explicitly rather than guessed at.

- current session schema version: **3**
- oldest readable session schema version: **2**

The version governs the whole document, including the objects nested inside it —
the run summary, the tool intents, the agent tasks.

### Configuration

A `config.toml` written by any 1.x build is readable by any later 1.x build.
Unknown keys are rejected loudly rather than ignored, so a typo is never silently
inert, and a version mismatch is an error rather than a guess.

- current config schema version: **1**

### Audit records

The audit trail is written to be read long after the build that produced it, by a
program that cannot ask which version it is parsing. Each record therefore names
its own shape in `schema_version`, and the committed field contract is enforced by
a test.

- current audit schema version: **1**
- contract: `internal/tool/testdata/audit_record_format.txt`

### Run summaries

The durable summary of a finished request names its own shape as well. It is
nested inside the session document, whose version governs the envelope, but a
summary is also read on its own — from a `last_run` field or an exported event —
and a reader there has no way to ask which build wrote it.

- current run summary schema version: **1**
- contract: `internal/session/testdata/run_summary_format.txt`
- the in-memory report a host receives (`agent.RunReport`) carries the same
  version under its own constant; its contract is
  `internal/agent/testdata/run_report_format.txt`

### Program-facing output

`--output json` and `--output jsonl` are the documented way to drive Eylu from a
script, so their shape is promised too.

- every line of `--output jsonl` carries `type` and `schema_version`
- the object `--output json` writes carries `schema_version` and keeps every
  field of the model response under the name it already had
- current envelope schema version: **1**
- contracts: `internal/app/testdata/json_response_format.txt`,
  `internal/app/testdata/json_error_format.txt`,
  `internal/app/testdata/jsonl_line_format.txt`

### Permission mode semantics

`manual`, `plan`, `auto` and `full` are a user's mental model of what Eylu is
allowed to do without asking. Their meanings are frozen:

- **narrowing** a mode — asking for a confirmation that used to be skipped, or
  refusing a call that used to run — is a security fix and may happen in a patch
  release;
- **widening** a mode — running without a confirmation something that used to
  require one — is a breaking change, because a user who granted trust under the
  old meaning did not grant it under the new one.

This is why a change to what `auto` or `full` may do is decided before 1.0 rather
than after it.

## Not promised

### The Go API

Every package lives under `internal/`, so nothing here can be imported by another
module. This is a deliberate choice and not an oversight: it means 1.x is free to
restructure, rename and split its own packages without that being a compatibility
event, and it is what lets a structural refactor land after 1.0 without a major
version bump.

If a public Go API is ever wanted, it will be a new, purpose-built package with
its own contract — not `internal/` accidentally made visible.

### Presentation

The following may change in any release, including a patch release:

- the text, layout, colours and key bindings of the terminal interface;
- the wording of log lines, diagnostics and error messages;
- the concrete shape of identifiers and timestamps the output happens to use
  (for example the exact form of a generated request ID or session ID);
- which entries a UI chooses to display, and in what order.

The architecture document already classes these as presentation
(`docs/architecture-and-reliability.md` §5); this section restates it as a promise
about what is *not* promised.

## How the promise is enforced

A promise that nothing checks is a sentence, not a contract. Each format above has
a committed field contract under `testdata/`, and
`internal/docscheck.CheckFieldContract` compares the struct tags the encoder
actually uses against it:

- a field that disappeared was **removed or renamed** and fails the build;
- a field that appeared is an **addition**, and is recorded in the same change, so
  the diff of the contract file is the review of the format change;
- a field that changed between required and optional fails, because that changes
  what a reader can assume about its presence.

Regenerate a contract deliberately with `EYLU_UPDATE_GOLDEN=1 go test ./...`. The
regeneration is expected to be a pure addition; anything else is the breaking
change this document says will not happen.

## Known gaps in this commitment

- The audit record and the run summary are versioned as of 1.0. A record written
  by an earlier release candidate has no `schema_version` field; a reader must
  treat a missing version as "written before the field existed" rather than as
  version 0.
- The envelope version covers the lines the CLI writes. It does not describe the
  shape of provider payloads Eylu relays inside those lines, which belong to the
  provider.
