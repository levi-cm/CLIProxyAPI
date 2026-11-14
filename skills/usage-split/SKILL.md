---
name: usage-split
description: "Use when orchestrating subagents: tells you how many more subagents the whole fleet has room for and which model, effort and mode to use, from CLIProxyAPI's live quota."
---

# Usage Split

Run `bash <skill-dir>/scripts/advise.sh` (no key needed) and follow it:

- "room for ~N more agents" is shared by all orchestrators on all machines: start at most N new subagents. "hold"/"conserve": start none.
- Model lines are in preference order. Use the model named for your harness (e.g. `claude-code: opus`) with the exact `effort=` and `mode=`. Skip lines without your harness or marked hold. Never raise effort or pick fast/priority yourself; the advice climbs its ladder when the fleet has slack.
- "OpenCode Go: avoid until …": don't use OpenCode Go models until then.
- Reuse the advice until "reuse until" (5 min), then ask again before starting more.
- "unavailable" or exit 3: start no new subagents. Never use Fable models.
