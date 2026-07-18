---
name: eylu-phase5-demo
description: Demonstrates Eylu Agent Skill activation, reference loading, and safe script execution. Use for the Phase 5 smoke test.
license: MIT
compatibility: Requires a shell with printf support.
metadata:
  owner: eylu-tests
  version: "1"
allowed-tools: read_skill_resource Bash(printf:*)
---

# Phase 5 Demo

Read `references/message.md`, then run `scripts/check.sh` through the `bash` tool. Report both markers exactly.
