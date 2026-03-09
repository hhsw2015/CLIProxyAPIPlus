import importlib.util
import json
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock


MODULE_PATH = Path(__file__).with_name("classify-auth.py")
SPEC = importlib.util.spec_from_file_location("classify_auth", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC is not None and SPEC.loader is not None
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


def make_args(**overrides):
    defaults = {
        "auth_dir": "~/.cli-proxy-api",
        "base_url": MODULE.DEFAULT_CODEX_BASE_URL,
        "quota_path": "/responses",
        "model": "gpt-5",
        "timeout": 20,
        "workers": 4,
        "retry_attempts": 3,
        "retry_backoff": 0.6,
        "refresh_before_check": False,
        "refresh_url": MODULE.DEFAULT_REFRESH_URL,
    }
    defaults.update(overrides)
    return types.SimpleNamespace(**defaults)


class ClassifyAuthTests(unittest.TestCase):
    def test_build_scan_plan_dedupes_active_and_limit(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            auth_dir = Path(temp_dir)
            limit_dir = auth_dir / "limit"
            limit_dir.mkdir()

            active_old = auth_dir / "codex-old.json"
            active_new = auth_dir / "codex-new.json"
            limit_dup = limit_dir / "codex-limit.json"
            limit_unique = limit_dir / "codex-unique.json"

            payload_dup = {
                "type": "codex",
                "email": "dup@example.com",
                "refresh_token": "dup-refresh",
                "access_token": "dup-access",
            }
            payload_unique = {
                "type": "codex",
                "email": "unique@example.com",
                "refresh_token": "unique-refresh",
                "access_token": "unique-access",
            }

            active_old.write_text(json.dumps(payload_dup), encoding="utf-8")
            active_new.write_text(json.dumps(payload_dup), encoding="utf-8")
            limit_dup.write_text(json.dumps(payload_dup), encoding="utf-8")
            limit_unique.write_text(json.dumps(payload_unique), encoding="utf-8")

            # Ensure codex-new is the freshest active copy.
            active_old.touch()
            active_new.touch()

            active_targets, limit_targets = MODULE._build_scan_plan(auth_dir, limit_dir)

            self.assertEqual([path.name for path in active_targets], ["codex-new.json"])
            self.assertEqual([path.name for path in limit_targets], ["codex-unique.json"])

    def test_scan_single_file_recovers_refreshable_401(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            auth_path = Path(temp_dir) / "codex.json"
            auth_path.write_text(
                json.dumps(
                    {
                        "type": "codex",
                        "email": "alive@example.com",
                        "refresh_token": "refresh-token",
                        "access_token": "stale-token",
                    }
                ),
                encoding="utf-8",
            )

            args = make_args()

            with mock.patch.object(
                MODULE,
                "_probe_once",
                side_effect=[
                    (401, '{"error":"unauthorized"}', ""),
                    (200, '{"quota": {"remaining": 1}}', ""),
                ],
            ), mock.patch.object(
                MODULE,
                "_try_refresh_token",
                return_value=(
                    "alive",
                    MODULE.RefreshedTokenData(
                        access_token="fresh-token",
                        refresh_token="fresh-refresh",
                        id_token="header.payload.sig",
                        email="alive@example.com",
                        account_id="acct-123",
                        expired="2099-01-01T00:00:00Z",
                    ),
                    "",
                ),
            ), mock.patch.object(
                MODULE,
                "_check_access_token",
                return_value=("alive", ""),
            ):
                result = MODULE._scan_single_file(auth_path, args)[0]

            self.assertEqual(result.status_code, 200)
            self.assertFalse(result.unauthorized_401)
            self.assertFalse(result.delete_invalid)
            updated = json.loads(auth_path.read_text(encoding="utf-8"))
            self.assertEqual(updated["access_token"], "fresh-token")
            self.assertEqual(updated["refresh_token"], "fresh-refresh")
            self.assertEqual(updated["account_id"], "acct-123")
            self.assertEqual(updated["email"], "alive@example.com")
            self.assertEqual(updated["expired"], "2099-01-01T00:00:00Z")
            self.assertIn("last_refresh", updated)

    def test_scan_single_file_deletes_confirmed_deleted_401(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            auth_path = Path(temp_dir) / "codex.json"
            auth_path.write_text(
                json.dumps(
                    {
                        "type": "codex",
                        "email": "dead@example.com",
                        "refresh_token": "refresh-token",
                        "access_token": "stale-token",
                    }
                ),
                encoding="utf-8",
            )

            args = make_args()

            with mock.patch.object(
                MODULE,
                "_probe_once",
                return_value=(401, '{"error":"unauthorized"}', ""),
            ), mock.patch.object(
                MODULE,
                "_try_refresh_token",
                return_value=("deleted", None, "account_deleted"),
            ):
                result = MODULE._scan_single_file(auth_path, args)[0]

            self.assertEqual(result.status_code, 401)
            self.assertTrue(result.unauthorized_401)
            self.assertTrue(result.delete_invalid)


if __name__ == "__main__":
    unittest.main()
