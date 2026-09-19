# Security policy

Eylu runs commands, edits files, connects to remote MCP servers and sends
repository content to a model. A security report about it is taken seriously, and
this file exists so that there is one obvious way to make one.

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Use GitHub's private reporting flow: go to the repository's **Security** tab and
choose **Report a vulnerability**. That opens a private advisory visible only to
the maintainers, and it is the same channel used to publish a fix.

If you cannot use GitHub advisories, open a normal issue that says only that you
have a security report and how to reach you, without describing the problem. A
maintainer will move the conversation to a private channel.

A useful report includes:

- what an attacker gains, and what they must already be able to do to gain it;
- the affected version or commit, and the platform;
- the smallest reproduction you have, including the configuration and the
  permission mode (`manual`, `plan`, `auto` or `full`) it was run under;
- whether it is already public anywhere.

## What is in scope

Anything that lets something outside the conversation change what Eylu does:

- a tool result, file, MCP server, skill or web page that reaches the model as
  untrusted data and nevertheless gets executed as an instruction;
- an MCP server that gains authority the local configuration did not grant, or a
  tool that stands in for a built-in one;
- a subagent that runs with more authority than the request that delegated to it,
  or that escapes the resource coordinator or the checkpoint;
- a command that the policy layer classifies as read-only and that then writes,
  moves, deletes or executes something;
- a path that escapes the workspace through symlinks, junctions, `..` or a
  platform-specific path form;
- a credential, token or secret that is written to a log, an audit record, a
  session file or an error message;
- a dependency Eylu ships with a known, exploitable CVE;
- a build or release artifact that can be substituted without detection.

## What is out of scope

- **What the model decides to do with content it was given.** Eylu frames content
  it read from outside the conversation and says so in the system prompt
  (`docs/architecture-and-reliability.md` §6), but a model can still choose to
  follow text that is framed as data. That is a property of the model, not a
  vulnerability in Eylu, unless Eylu failed to frame the content or lost the
  framing on the way.
- **Anything that already requires the ability to run code as the user.** Eylu is
  an application-layer permission system, not an operating system sandbox; a
  command it runs has the privileges of the process that ran it. This boundary is
  stated in `docs/architecture-and-reliability.md` §7.
- **Running Eylu with `full` permission on untrusted content by choice.** The
  modes are documented and their meanings are frozen in `docs/compatibility.md`.
- Reports produced only by a scanner, with no path from the report to something an
  attacker gains.

## Supported versions

| version | supported |
|---|---|
| the latest 1.x release | yes |
| the previous 1.x minor | security fixes only |
| anything older, or an unreleased commit | no |

Fixes are released as a new patch version. A fix that narrows a permission mode is
a security fix and may ship in a patch (see `docs/compatibility.md`); a fix that
widens one is not made without a major version decision.

## What to expect

- **Acknowledgement** within a few days.
- **An assessment** — in scope or not, and how severe — within about a week.
- **A fix and a release** for anything exploitable, with the reporter credited if
  they want to be.
- **A published advisory** once a fixed release exists, describing the issue and
  the affected versions.

This is a small project without a paid security team; the timings above are
intentions rather than a service-level agreement, and a report that is being
actively exploited will jump ahead of them.

## What Eylu already does

These are the defences a report is measured against. They are stated here so a
reporter can tell a gap from a design decision, and each one is enforced by a test
named after the guarantee in `docs/architecture-and-reliability.md` §8:

- an MCP server cannot declare its own tool read-only, and its tool names cannot
  collide with a built-in's;
- only a tool the host registered itself can raise a request-level control state;
- a subagent runs under its parent's permission mode, cannot delegate again,
  cannot ask the user a question, cannot overwrite an existing file, and shares
  its parent's resource coordinator and checkpoint;
- content read from outside the conversation is delivered to the model inside an
  untrusted envelope;
- the command classifier reads a command line with the rules of the shell that
  will run it, and anything it cannot prove needs confirmation;
- the release artifacts are signed with cosign, and dependencies are scanned with
  `govulncheck` on every change.
