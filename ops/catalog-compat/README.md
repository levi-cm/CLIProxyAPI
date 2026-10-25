# Codex catalog compatibility refresh

This is a live workaround for the v8.0.15-based router. Its catalog validator
and client-model builder require a `gpt-5.5` default template, which the official
remote catalog no longer contains. Upstream commit `e240bd84` removes this
requirement and adds fallback-template selection, but deploying that binary
requires a separate production rollout.

`refresh_catalog.py` downloads the official Codex client catalog, validates the
fields required by the deployed version, and preserves all upstream entries and
metadata. If `gpt-5.5` is absent, it appends the original template extracted from
`internal/registry/models/codex_client_models.json` at fork commit `17199e47`.
It does not add that model to the accounts' advertised model list.

The helper only publishes a fully validated file, using atomic replacement.
Network errors, invalid data, and overlapping invocations retain the last good
copy. An unchanged download does not rewrite the file. It does not access proxy
credentials, conversation bindings, or account-policy state.

## Local installation

The installed helper and original legacy template live in
`/home/levi/.local/share/cliproxy-catalog/`. The output is
`/DATA/AppData/CLIProxyAPI/static/catalogs/codex-client-compat.json`, visible in the
existing container mount as `/CLIProxyAPI/static/catalogs/codex-client-compat.json`.
A user crontab entry refreshes the file hourly and logs metadata to the system
journal under `cliproxy-catalog`. The router reads the file on its existing
three-hour catalog refresh cadence.

Only this setting is added to the production configuration:

```yaml
models:
    codex-catalog: /CLIProxyAPI/static/catalogs/codex-client-compat.json
```

Changing the source triggers an immediate refresh through the existing config
watcher; no process restart is required. This old version also replaces executors
on configuration reload, so first confirm no upstream WebSocket sessions will
be interrupted. The checked deployment uses HTTP upstream transport. Subsequent
hourly file updates do not change configuration or replace executors.

Configuration backups are private under the installed helper's `backups/`.
Retain the configuration inode when editing it because Docker bind-mounts the
file. Do not replace it using an atomic rename on the host.

## Checks and retirement

```bash
python3 -m unittest discover -s ops/catalog-compat -v
```

Validate the generated JSON with the running version's Go registry validator in
an isolated checkout before changing the live source. Verify both ordinary and
Codex-client `/v1/models` responses and uninterrupted access on all three bound
interfaces during the reload. Verify active inference completes and conversation
continuations still work.

Once a tested binary containing `e240bd84` is deployed, remove the source override
and the marked `cliproxy-catalog-compat` crontab block, then retire the helper and
compatibility JSON. Do not restore the old source on the current binary expecting
the refresh failure to be fixed; that reintroduces the original incompatibility.
