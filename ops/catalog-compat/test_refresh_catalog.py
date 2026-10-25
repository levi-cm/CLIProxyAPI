import copy
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    "refresh_catalog", Path(__file__).with_name("refresh_catalog.py")
)
catalog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(catalog)


def model(slug):
    return {
        "slug": slug,
        "display_name": slug,
        "description": "Fixture model",
        "base_instructions": "Preserve these instructions exactly.",
        "minimal_client_version": "0.160.0",
        "visibility": "list",
        "default_reasoning_level": "high",
        "context_window": 1000,
        "max_context_window": 2000,
        "priority": 0,
        "supported_reasoning_levels": [{"effort": "high"}],
    }


class RefreshCatalogTests(unittest.TestCase):
    def test_preserves_upstream_models_and_unknown_fields(self):
        upstream = {"models": [model("current")], "future_metadata": {"v": 2}}
        original = copy.deepcopy(upstream)
        result = catalog.build_catalog(upstream, model("gpt-5.5"))
        self.assertEqual(upstream, original)
        self.assertEqual(result["models"][0], original["models"][0])
        self.assertEqual(result["future_metadata"], original["future_metadata"])
        self.assertEqual(result["models"][1], model("gpt-5.5"))

    def test_upstream_legacy_model_takes_precedence(self):
        upstream_model = model("gpt-5.5")
        upstream_model["description"] = "New upstream description"
        upstream = {"models": [upstream_model]}
        self.assertEqual(catalog.build_catalog(upstream, model("gpt-5.5")), upstream)

    def test_rejects_invalid_catalogs(self):
        duplicate = {"models": [model("current"), model("current")]}
        bad_window = model("current")
        bad_window["context_window"] = 3000
        bad_effort = model("current")
        bad_effort["default_reasoning_level"] = "unsupported"
        for upstream in [
            {}, {"models": []}, duplicate,
            {"models": [bad_window]}, {"models": [bad_effort]},
        ]:
            with self.subTest(upstream=upstream):
                with self.assertRaises(ValueError):
                    catalog.build_catalog(upstream, model("gpt-5.5"))

    def test_failed_download_or_validation_keeps_last_good_file(self):
        with tempfile.TemporaryDirectory() as folder:
            target = Path(folder) / "catalog.json"
            fallback = Path(folder) / "fallback.json"
            target.write_bytes(b"last good catalog")
            fallback.write_text(json.dumps(model("gpt-5.5")))
            def failed_fetch(_):
                raise OSError("offline")
            for fetch in [failed_fetch, lambda _: io.BytesIO(b"invalid JSON")]:
                with self.assertRaises((OSError, ValueError)):
                    catalog.refresh("https://fixture.invalid/catalog", fallback, target, fetch)
                self.assertEqual(target.read_bytes(), b"last good catalog")
            self.assertEqual(sorted(p.name for p in Path(folder).iterdir()),
                             ["catalog.json", "fallback.json"])

    def test_refresh_publishes_valid_catalog_and_avoids_identical_rewrites(self):
        with tempfile.TemporaryDirectory() as folder:
            target = Path(folder) / "catalog.json"
            fallback = Path(folder) / "fallback.json"
            fallback.write_text(json.dumps(model("gpt-5.5")))
            raw = json.dumps({"models": [model("current")]}).encode()
            fetch = lambda _: io.BytesIO(raw)
            self.assertTrue(catalog.refresh("https://fixture.invalid/catalog", fallback, target, fetch))
            published = json.loads(target.read_bytes())
            self.assertEqual([m["slug"] for m in published["models"]], ["current", "gpt-5.5"])
            before = target.stat()
            self.assertFalse(catalog.refresh("https://fixture.invalid/catalog", fallback, target, fetch))
            self.assertEqual(target.stat().st_ino, before.st_ino)
            self.assertEqual(target.stat().st_mtime_ns, before.st_mtime_ns)


if __name__ == "__main__":
    unittest.main()
