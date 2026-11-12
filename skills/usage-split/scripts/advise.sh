#!/usr/bin/env bash
# Print CLIProxyAPI subagent advice (no key needed).
curl -fsS --max-time 15 "${USAGE_SPLIT_URL:-http://localhost:8317}/v1/account-policy/subagent-advice?format=text" || { echo "usage-split: no advice; do not start extra subagents" >&2; exit 3; }
