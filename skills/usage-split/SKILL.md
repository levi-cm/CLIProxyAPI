---
name: usage-split
description: "Use when orchestrating subagents: tells you which model, effort and mode subagents should use and how freely to spend, from CLIProxyAPI's live quota."
---

# Usage Split

Run `bash <skill-dir>/scripts/advise.sh` (no key needed) and follow it:

- The first line says how freely to spend. You decide how many subagents the work needs; "spend freely" means many in parallel are welcome.
- Model lines are in preference order. Start each subagent with the model named for your harness (e.g. `claude-code: opus`) and the given effort, set explicitly. Skip lines without your harness.
- Stay well within each line's task budget. Use fast/priority mode only if your harness supports it.
- Reuse the advice until its "reuse until" time; ask earlier only after rate-limit errors.
- "unavailable" or exit code 3: start no new subagents.
- Never use Fable models for subagents.
