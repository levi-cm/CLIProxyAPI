#!/usr/bin/env bash
set -euo pipefail

models_repository="${MODELS_REPOSITORY_URL:-https://github.com/router-for-me/models.git}"
models_ref="${MODELS_REPOSITORY_REF:-main}"
catalog_dir="${MODEL_CATALOG_DIR:-internal/registry/models}"
codex_catalog="$catalog_dir/codex_client_models.json"
catalog_build_dir="$(mktemp -d)"
codex_candidate="$catalog_build_dir/codex-candidate.json"
catalog_validator="$catalog_build_dir/validate-codex-models"
trap 'rm -rf "$catalog_build_dir"' EXIT

./scripts/go-limited.sh build -o "$catalog_validator" ./cmd/validate_codex_models

git fetch --depth 1 "$models_repository" "$models_ref"
git show FETCH_HEAD:models.json > "$catalog_dir/models.json"

if git show FETCH_HEAD:codex_client_models.json > "$codex_candidate" &&
  "$catalog_validator" --file "$codex_candidate"; then
	mv "$codex_candidate" "$codex_catalog"
	printf 'Refreshed validated Codex client model catalog.\n'
else
	printf '::warning::Remote Codex client model catalog is missing or invalid; using embedded fallback.\n'
fi

"$catalog_validator" --file "$codex_catalog"
