#!/usr/bin/env python3
"""
Tests for gh-app-token.py and register-gh-app.py: private token file at open
time, strict installation-account matching, loopback-only callback with state
checking.

Run with:
    python3 -m unittest scripts/test_gh_app_scripts.py -v

The GitHub HTTP layer and JWT signing are stubbed; nothing calls GitHub.
"""

import importlib.util
import json
import os
import stat
import sys
import tempfile
import threading
import types
import unittest
import urllib.error
import urllib.request
from unittest import mock

SCRIPTS_DIR = os.path.dirname(os.path.abspath(__file__))

try:
    import jwt  # noqa: F401
except ImportError:
    # The token script only needs jwt.encode; stub it so the tests run without PyJWT.
    sys.modules["jwt"] = types.SimpleNamespace(encode=lambda *a, **k: "stub-jwt")


def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, os.path.join(SCRIPTS_DIR, filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


token_mod = _load("gh_app_token", "gh-app-token.py")
register_mod = _load("register_gh_app", "register-gh-app.py")


class GhAppTokenTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.config_dir = self._tmp.name
        self.app_json = os.path.join(self.config_dir, "gh-app.json")
        self.pem = os.path.join(self.config_dir, "gh-app.pem")
        self.token_file = os.path.join(self.config_dir, "github_token")
        with open(self.pem, "w") as f:
            f.write("not-a-real-key")
        patches = [
            mock.patch.object(token_mod, "CONFIG_DIR", self.config_dir),
            mock.patch.object(token_mod, "APP_JSON_PATH", self.app_json),
            mock.patch.object(token_mod, "TOKEN_FILE_PATH", self.token_file),
            mock.patch.object(token_mod, "generate_jwt", return_value="app-jwt"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)

    def _write_meta(self, **extra):
        meta = {"app_id": 1, "slug": "summa42-bot", "pem_path": self.pem}
        meta.update(extra)
        with open(self.app_json, "w") as f:
            json.dump(meta, f)

    def _run_main(self, argv=("gh-app-token.py",)):
        with mock.patch.object(sys, "argv", list(argv)):
            token_mod.main()

    def test_token_file_created_with_mode_0600_at_open_time(self):
        self._write_meta(installation_id=42)
        old_umask = os.umask(0o000)  # a permissive umask would yield 0666 if the mode were not passed to open()
        self.addCleanup(os.umask, old_umask)
        real_open = os.open
        with mock.patch.object(token_mod, "get_installation_token",
                               return_value={"token": "ghs_test", "expires_at": "later"}), \
             mock.patch.object(token_mod.os, "open", wraps=real_open) as open_spy:
            self._run_main()
        # The token file must be opened with 0600 in the same call, not chmod'd afterwards.
        open_spy.assert_any_call(self.token_file, mock.ANY, 0o600)
        self.assertEqual(stat.S_IMODE(os.stat(self.token_file).st_mode), 0o600)
        with open(self.token_file) as f:
            self.assertEqual(f.read(), "ghs_test")

    def test_missing_expected_account_is_an_error_not_first_installation(self):
        self._write_meta()
        installations = [{"id": 7, "account": {"login": "someone-else"}}]
        with mock.patch.object(token_mod, "get_installations", return_value=installations), \
             mock.patch.object(token_mod, "get_installation_token") as get_tok:
            with self.assertRaises(SystemExit) as cm:
                self._run_main()
        self.assertEqual(cm.exception.code, 1)
        get_tok.assert_not_called()
        self.assertFalse(os.path.exists(self.token_file))

    def test_expected_account_selected_even_when_not_first(self):
        self._write_meta()
        installations = [
            {"id": 7, "account": {"login": "someone-else"}},
            {"id": 9, "account": {"login": "SofiaFlux"}},
        ]
        with mock.patch.object(token_mod, "get_installations", return_value=installations), \
             mock.patch.object(token_mod, "get_installation_token",
                               return_value={"token": "ghs_x", "expires_at": "later"}) as get_tok:
            self._run_main()
        get_tok.assert_called_once_with(9, "app-jwt")

    def test_refresh_skips_when_token_is_fresh(self):
        with open(self.token_file, "w") as f:
            f.write("ghs_fresh")
        with mock.patch.object(token_mod, "get_installation_token") as get_tok, \
             mock.patch.object(token_mod, "get_installations") as get_inst:
            self._run_main(["gh-app-token.py", "--refresh-if-older-than", "50m"])
        get_tok.assert_not_called()
        get_inst.assert_not_called()
        with open(self.token_file) as f:
            self.assertEqual(f.read(), "ghs_fresh")


class RegisterGhAppTests(unittest.TestCase):
    def setUp(self):
        register_mod.captured_code = None
        register_mod.server_done = threading.Event()
        self.server = register_mod.make_server("expected-state", port=0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self._stop)

    def _stop(self):
        self.server.shutdown()
        self.server.server_close()

    def _get(self, query):
        url = f"http://127.0.0.1:{self.server.server_address[1]}/callback?{query}"
        try:
            with urllib.request.urlopen(url, timeout=5) as resp:
                return resp.status
        except urllib.error.HTTPError as e:
            return e.code

    def test_server_binds_loopback_only(self):
        self.assertEqual(self.server.server_address[0], "127.0.0.1")

    def test_wrong_state_is_rejected(self):
        self.assertEqual(self._get("code=abc&state=wrong"), 400)
        self.assertIsNone(register_mod.captured_code)
        self.assertFalse(register_mod.server_done.is_set())

    def test_missing_state_is_rejected(self):
        self.assertEqual(self._get("code=abc"), 400)
        self.assertIsNone(register_mod.captured_code)

    def test_correct_state_captures_code(self):
        self.assertEqual(self._get("code=abc&state=expected-state"), 200)
        self.assertEqual(register_mod.captured_code, "abc")
        self.assertTrue(register_mod.server_done.is_set())


if __name__ == "__main__":
    unittest.main()
