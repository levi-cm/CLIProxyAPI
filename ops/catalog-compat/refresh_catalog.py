#!/usr/bin/env python3
"""Refresh a compatible Codex catalog without modifying the running proxy."""

import argparse
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import sys
import tempfile
import urllib.request


SOURCE = "https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json"
MAX_BYTES = 8 << 20
LEGACY_SLUG = "gpt-5.5"


def validate(payload):
    """Match the required fields in this deployed version's Go validator."""
    if not isinstance(payload, dict) or not isinstance(payload.get("models"), list) or not payload["models"]:
        raise ValueError("catalog must contain models")
    seen = set()
    for model in payload["models"]:
        if not isinstance(model, dict):
            raise ValueError("model must be an object")
        for key in ["slug", "display_name", "description", "base_instructions",
                    "minimal_client_version", "visibility", "default_reasoning_level"]:
            if not isinstance(model.get(key), str) or not model[key].strip():
                raise ValueError("missing model string field: " + key)
        slug = model["slug"].strip()
        if slug in seen:
            raise ValueError("duplicate model slug")
        seen.add(slug)
        for key in ["context_window", "max_context_window", "priority"]:
            value = model.get(key)
            minimum = 0 if key == "priority" else 1
            if (isinstance(value, bool) or not isinstance(value, (int, float))
                    or not math.isfinite(value) or value != int(value)
                    or not minimum <= value <= 2**63 - 1):
                raise ValueError("invalid model integer field: " + key)
        if model["context_window"] > model["max_context_window"]:
            raise ValueError("context window exceeds maximum")
        levels = model.get("supported_reasoning_levels")
        if not isinstance(levels, list) or not levels:
            raise ValueError("missing reasoning levels")
        efforts = []
        for level in levels:
            effort = level.get("effort") if isinstance(level, dict) else None
            if not isinstance(effort, str) or not effort.strip():
                raise ValueError("invalid reasoning effort")
            efforts.append(effort.strip())
        if len(set(efforts)) != len(efforts) or model["default_reasoning_level"].strip() not in efforts:
            raise ValueError("invalid default or duplicate reasoning effort")


def build_catalog(upstream, legacy_template):
    validate(upstream)
    result = {**upstream, "models": list(upstream["models"])}
    if not any(model["slug"].strip() == LEGACY_SLUG for model in result["models"]):
        validate({"models": [legacy_template]})
        if legacy_template["slug"] != LEGACY_SLUG:
            raise ValueError("incorrect legacy template")
        result["models"].append(legacy_template)
    return result


def refresh(source, fallback_path, output_path, fetch=urllib.request.urlopen):
    # Network or validation failures occur before touching the last good file.
    with fetch(source) as response:
        raw = response.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise ValueError("upstream catalog exceeds size limit")
    fallback = json.loads(Path(fallback_path).read_bytes())
    payload = build_catalog(json.loads(raw), fallback)
    encoded = (json.dumps(payload, ensure_ascii=False, separators=(",", ":"), allow_nan=False) + "\n").encode()
    if len(encoded) > MAX_BYTES:
        raise ValueError("compatible catalog exceeds size limit")
    target = Path(output_path)
    if target.is_symlink():
        raise ValueError("catalog destination must not be a symlink")
    if target.exists() and target.read_bytes() == encoded:
        return False
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=target.parent, prefix=".catalog-", delete=False) as handle:
            temporary = Path(handle.name)
            os.fchmod(handle.fileno(), 0o644)
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
        temporary = None
        directory = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--legacy-template", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    # Cron invocations never overlap, including a slow upstream download.
    with args.output.with_suffix(".lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print(json.dumps({"status": "already_running"}))
            return 0
        try:
            changed = refresh(SOURCE, args.legacy_template, args.output)
            data = args.output.read_bytes()
            print(json.dumps({"status": "updated" if changed else "unchanged",
                              "checked_at": datetime.now(timezone.utc).isoformat(),
                              "models": len(json.loads(data)["models"]),
                              "sha256": hashlib.sha256(data).hexdigest()}))
            return 0
        except (OSError, ValueError) as error:
            print(json.dumps({"status": "failed_kept_last_good", "error_type": type(error).__name__}), file=sys.stderr)
            return 1


if __name__ == "__main__":
    sys.exit(main())
