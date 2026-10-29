#!/usr/bin/env python3
"""Real local filesystem, process and HTTP tests; never restarts a real unit."""

import importlib.util
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("xui_release", Path(__file__).with_name("release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


RESTART = '''import json,os,pathlib,sys,time
state,plan,calls,marker = map(pathlib.Path,sys.argv[1:])
identity=os.environ.get('XUI_RELEASE_ID','')
with calls.open('a') as out: out.write(identity+'\\n')
options=json.loads(plan.read_text()).get(identity,{})
if options.get('block'):
 marker.write_text('ready')
 time.sleep(60)
if options.get('fail'):
 print('sensitive restart output',file=sys.stderr)
 sys.exit(1)
data={'release':options.get('release',identity),'schema_version':options.get('schema_version',16),'status':'ok'}
if options.get('unready') or not identity: data['status']='unavailable'
state.write_text(json.dumps(data))
'''


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="xui-release-test-")
        self.base = Path(self.directory.name)
        self.root = self.base / "managed"
        self.control = self.base / "xui-control"
        self.agent = self.base / "xui-agent"
        for executable in [self.control, self.agent]:
            executable.write_bytes(b"#!/bin/sh\nexit 0\n")
            executable.chmod(0o700)
        self.web = self.base / "web"
        (self.web / "assets").mkdir(parents=True)
        (self.web / "index.html").write_text("<html>fixture release</html>")
        (self.web / "assets" / "app.js").write_text("window.releaseFixture=true")
        self.state = self.base / "state.json"
        self.state.write_text(json.dumps({"status": "unavailable"}))
        self.plan = self.base / "plan.json"
        self.plan.write_text("{}")
        self.calls = self.base / "calls.txt"
        self.marker = self.base / "restart-started"
        self.restart_script = self.base / "restart.py"
        self.restart_script.write_text(RESTART)
        self.argv = [sys.executable, str(self.restart_script), str(self.state), str(self.plan), str(self.calls), str(self.marker)]
        state_path = self.state

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                value = json.loads(state_path.read_text())
                if value.get("redirect"):
                    self.send_response(302)
                    self.send_header("Location", "/not-readiness")
                    self.end_headers()
                    return
                body = ("x" * 4097).encode() if value.get("oversize") else json.dumps(value).encode()
                self.send_response(200 if value.get("status") == "ok" else 503)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                try:
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass  # A bounded readiness request may already have timed out.

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}/readyz"
        self.addCleanup(self.directory.cleanup)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def pack(self, identity, **kwargs):
        return release.pack(self.root, identity, self.control, self.agent, self.web, **kwargs)

    def activate_old(self):
        self.pack("old")
        result = release.activate(self.root, "old", self.argv, self.url, .3)
        self.assertEqual(result["state"], "active")

    def assert_code(self, code, fn, *args, **kwargs):
        with self.assertRaises(release.ReleaseError) as raised:
            fn(*args, **kwargs)
        self.assertEqual(raised.exception.code, code)

    def test_pack_private_manifest_and_executable_integrity(self):
        manifest = self.pack("release-0.16")
        self.assertEqual(len(manifest["artifacts"]), 4)
        self.assertEqual(manifest["compatibility_epoch"], "staff-mfa-v1")
        self.assertEqual(release.verify(self.root, "release-0.16"), manifest)
        for path in [self.root, self.root / "releases", self.root / "releases/release-0.16/web/assets"]:
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE((self.root / "releases/release-0.16/release.json").stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE((self.root / "releases/release-0.16/bin/xui-control").stat().st_mode), 0o700)
        self.assert_code("release_exists", self.pack, "release-0.16")

    def test_rejects_symlink_inputs_hidden_web_files_and_unsafe_ids(self):
        self.assert_code("invalid_release_id", self.pack, "../outside")
        (self.web / "assets" / "external.js").symlink_to(self.control)
        self.assert_code("symlink_path", self.pack, "linked-artifact")
        (self.web / "assets" / "external.js").unlink()
        (self.web / ".env").write_text("not-a-release-artifact")
        self.assert_code("invalid_artifact_path", self.pack, "hidden-web")
        (self.web / ".env").unlink()
        alias = self.base / "root-link"
        alias.symlink_to(self.root)
        self.assert_code("symlink_path", release.managed_root, alias)

    def test_tampered_file_unlisted_file_and_manifest_escape_are_rejected(self):
        self.pack("tampered")
        (self.root / "releases/tampered/web/index.html").write_text("modified")
        self.assert_code("artifact_digest_mismatch", release.verify, self.root, "tampered")
        self.pack("extra")
        extra = self.root / "releases/extra/web/extra.js"
        extra.write_text("unexpected")
        extra.chmod(0o600)
        self.assert_code("unlisted_artifact", release.verify, self.root, "extra")
        self.pack("escaped")
        path = self.root / "releases/escaped/release.json"
        manifest = json.loads(path.read_text())
        manifest["artifacts"][0]["path"] = "../../outside"
        release.atomic_json(path, manifest)
        self.assert_code("invalid_artifact_path", release.verify, self.root, "escaped")

    def test_successful_activation_checks_identity_and_same_release_is_idempotent(self):
        self.activate_old()
        self.pack("candidate")
        result = release.activate(self.root, "candidate", self.argv, self.url, .3)
        self.assertEqual(result, {"state": "active", "release": "candidate", "previous": "old", "schema_version": 16})
        self.assertEqual(release.current_release(self.root), "candidate")
        self.assertFalse((self.root / "pending.json").exists())
        repeated = release.activate(self.root, "candidate", self.argv, self.url, .3)
        self.assertEqual(repeated["state"], "already_active")
        self.assertEqual(self.calls.read_text().splitlines(), ["old", "candidate"])

    def test_old_healthy_response_does_not_approve_candidate_and_rolls_back(self):
        self.activate_old()
        self.pack("candidate")
        self.plan.write_text(json.dumps({"candidate": {"release": "old"}}))
        self.assert_code("activation_failed_rolled_back", release.activate,
                         self.root, "candidate", self.argv, self.url, .15)
        self.assertEqual(release.current_release(self.root), "old")
        self.assertEqual(release.probe(self.url)["release"], "old")
        self.assertEqual(self.calls.read_text().splitlines(), ["old", "candidate", "old"])
        self.assertFalse((self.root / "pending.json").exists())

    def test_epoch_and_unsupported_forward_schema_are_rejected_before_switch(self):
        self.activate_old()
        self.pack("legacy", epoch="pre-mfa")
        self.assert_code("incompatible_epoch", release.activate,
                         self.root, "legacy", self.argv, self.url, .15)
        self.pack("schema17", schema_version=17, schema_min=16, schema_max=17)
        self.assert_code("incompatible_schema_range", release.activate,
                         self.root, "schema17", self.argv, self.url, .15)
        self.assertEqual(release.current_release(self.root), "old")
        self.assertEqual(self.calls.read_text().splitlines(), ["old"])
        self.assertFalse((self.root / "pending.json").exists())

    def test_readiness_requires_declared_migration_target(self):
        manifest = self.pack("schema17", schema_version=17, schema_min=16, schema_max=17)
        self.state.write_text(json.dumps({"status": "ok", "release": "schema17", "schema_version": 16}))
        self.assert_code("release_not_ready", release.wait_ready, self.url, manifest, .1)
        self.state.write_text(json.dumps({"status": "ok", "release": "schema17", "schema_version": 17}))
        self.assertEqual(release.wait_ready(self.url, manifest, .1)["schema_version"], 17)

    def test_unexpected_schema_blocks_incompatible_binary_rollback(self):
        self.activate_old()
        self.pack("candidate")
        self.plan.write_text(json.dumps({"candidate": {"schema_version": 17}}))
        self.assert_code("activation_failed_recovery_required", release.activate,
                         self.root, "candidate", self.argv, self.url, .1)
        self.assertEqual(release.current_release(self.root), "candidate")
        self.assertTrue((self.root / "pending.json").exists())
        self.assertEqual(self.calls.read_text().splitlines(), ["old", "candidate"])
        self.assert_code("rollback_schema_incompatible", release.recover,
                         self.root, self.argv, self.url, .1)

    def test_inventory_limits_and_parent_symlinks_are_rejected(self):
        previous = release.MAX_FILES
        try:
            release.MAX_FILES = 3
            self.assert_code("release_too_large", self.pack, "bounded")
        finally:
            release.MAX_FILES = previous
        self.pack("parent-link")
        assets = self.root / "releases/parent-link/web/assets"
        target = self.base / "outside-assets"
        assets.rename(target)
        assets.symlink_to(target)
        self.assert_code("symlink_path", release.verify, self.root, "parent-link")
        for value in ["web/" + "a/" * 17 + "x.js", "web/" + "x" * 1025]:
            self.assert_code("invalid_artifact_path", release.safe_artifact_path, value)

    def test_restart_failure_cli_is_sanitized_and_restores_previous(self):
        self.activate_old()
        self.pack("candidate")
        self.plan.write_text(json.dumps({"candidate": {"fail": True}}))
        command = [sys.executable, str(Path(release.__file__)), "activate", "--root", str(self.root),
                   "--release", "candidate", "--restart-json", json.dumps(self.argv),
                   "--ready-url", self.url, "--timeout", ".15"]
        result = subprocess.run(command, capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stderr), {"ok": False, "error": "activation_failed_rolled_back"})
        self.assertNotIn("sensitive", result.stdout + result.stderr)
        self.assertNotIn(str(self.base), result.stdout + result.stderr)
        self.assertEqual(release.current_release(self.root), "old")

    def test_failed_rollback_leaves_recovery_journal_then_recovers(self):
        self.activate_old()
        self.pack("candidate")
        self.plan.write_text(json.dumps({"candidate": {"unready": True}, "old": {"fail": True}}))
        self.assert_code("activation_failed_recovery_required", release.activate,
                         self.root, "candidate", self.argv, self.url, .15)
        self.assertTrue((self.root / "pending.json").exists())
        self.assert_code("recovery_required", release.activate,
                         self.root, "candidate", self.argv, self.url, .15)
        self.plan.write_text("{}")
        result = release.recover(self.root, self.argv, self.url, .3)
        self.assertEqual(result["release"], "old")
        self.assertFalse((self.root / "pending.json").exists())
        self.assertEqual(release.probe(self.url)["release"], "old")

    def test_sigkill_holds_lock_and_recovers_even_if_candidate_is_corrupted(self):
        self.activate_old()
        self.pack("candidate")
        self.plan.write_text(json.dumps({"candidate": {"block": True}}))
        command = [sys.executable, str(Path(release.__file__)), "activate", "--root", str(self.root),
                   "--release", "candidate", "--restart-json", json.dumps(self.argv),
                   "--ready-url", self.url, "--timeout", "1"]
        child = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                 start_new_session=True)
        try:
            deadline = time.monotonic() + 5
            while not self.marker.exists() and time.monotonic() < deadline:
                time.sleep(.02)
            self.assertTrue(self.marker.exists(), "child did not reach switched restart boundary")
            self.assert_code("release_operation_busy", release.activate,
                             self.root, "old", self.argv, self.url, .15)
            os.killpg(child.pid, signal.SIGKILL)
            child.wait(timeout=2)
        finally:
            if child.poll() is None:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait(timeout=2)
        self.assertEqual(os.readlink(self.root / "current"), "releases/candidate")
        self.assertTrue((self.root / "pending.json").exists())
        (self.root / "releases/candidate/bin/xui-control").write_text("corrupted after crash")
        self.plan.write_text("{}")
        result = release.recover(self.root, self.argv, self.url, .3)
        self.assertEqual(result["release"], "old")
        self.assertEqual(release.current_release(self.root), "old")
        self.assertFalse((self.root / "pending.json").exists())

    def test_url_redirect_response_and_restart_validation(self):
        self.activate_old()
        self.pack("candidate")
        for value in ["http://example.test/readyz", "http://localhost/readyz", self.url + "?secret=1",
                      self.url.replace("/readyz", "/other"), self.url.replace("127.0.0.1", "user@127.0.0.1")]:
            with self.subTest(value=value):
                with self.assertRaises(release.ReleaseError):
                    release.ready_url(value)
        for argv in [[], "shell command", [""], [True]]:
            self.assert_code("invalid_restart_argv", release.activate,
                             self.root, "candidate", argv, self.url, .15)
        self.assertFalse((self.root / "pending.json").exists())
        self.state.write_text(json.dumps({"status": "ok", "release": "old", "schema_version": 16, "redirect": True}))
        self.assert_code("readiness_redirect", release.probe, self.url)
        self.state.write_text(json.dumps({"status": "ok", "release": "old", "schema_version": True}))
        self.assert_code("invalid_readiness", release.probe, self.url)
        self.state.write_text(json.dumps({"status": "ok", "release": "old", "schema_version": 16, "oversize": True}))
        self.assert_code("invalid_readiness", release.probe, self.url)

    def test_current_link_cannot_escape_managed_release_directory(self):
        self.pack("candidate")
        (self.root / "current").symlink_to(self.base)
        self.assert_code("invalid_current_link", release.activate,
                         self.root, "candidate", self.argv, self.url, .15)
        self.assertEqual(os.readlink(self.root / "current"), str(self.base))
        self.assertFalse((self.root / "pending.json").exists())


if __name__ == "__main__":
    unittest.main()
