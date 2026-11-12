---
name: usage-split
description: "Use when orchestrating subagents: tells you which model, effort and mode subagents should use and how freely to spend, from CLIProxyAPI's live quota."
---

# Usage Split

Run `bash <skill-dir>/scripts/advise.sh` (no key needed) and follow it:

- The first line says how freely to spend. You decide how many subagents the work needs; "spend freely" means many in parallel are welcome.
- Per provider, the "as one" line combines all accounts into one allowance: what is left now, what the next resets add, and what would expire unused. The goal is to use weekly (and monthly) allowances fully before they reset; 5h limits rarely matter. The "accounts" line has the per-account detail if needed.
- Model lines are in preference order. Start each subagent with the model named for your harness (e.g. `claude-code: opus`) and the given effort, set explicitly. Skip lines without your harness.
- Stay well within each line's task budget. Use fast/priority mode only if your harness supports it.
- Reuse the advice until its "reuse until" time; ask earlier only after rate-limit errors.
- "unavailable" or exit code 3: start no new subagents.
- Never use Fable models for subagents.
