#!/usr/bin/env python3
"""Validate the production Nginx template using private, loopback-only fixtures.

Requires Linux, Docker and OpenSSL. It never installs a host service, uses a
business database, or contacts a merchant. Only containers labelled with this
invocation's random identifier are removed. No third-party Python modules.
"""

import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


ROOT = Path(__file__).resolve().parent.parent
IMAGE = "nginx@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236"
LABEL = "com.xui.production-proxy-test"
HOST = "panel.example.com"


class CheckFailure(Exception):
    pass


def command(arguments, failure, timeout=30, combined=False):
    try:
        result = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise CheckFailure(failure) from None
    if result.returncode:
        raise CheckFailure(failure)
    return result.stdout + result.stderr if combined else result.stdout


def certificate(directory):
    common = ["openssl", "req", "-newkey", "rsa:2048", "-nodes"]
    command(common + ["-x509", "-keyout", str(directory / "ca.key"),
                      "-out", str(directory / "ca.crt"), "-days", "1",
                      "-subj", "/CN=XUI local proxy acceptance CA"], "fixture_ca_failed")
    command(common + ["-keyout", str(directory / "server.key"),
                      "-out", str(directory / "server.csr"),
                      "-subj", "/CN=" + HOST], "fixture_certificate_failed")
    extensions = directory / "server.ext"
    extensions.write_text("subjectAltName=DNS:" + HOST + "\nextendedKeyUsage=serverAuth\n")
    command(["openssl", "x509", "-req", "-in", str(directory / "server.csr"),
             "-CA", str(directory / "ca.crt"), "-CAkey", str(directory / "ca.key"),
             "-CAcreateserial", "-out", str(directory / "server.crt"), "-days", "1",
             "-extfile", str(extensions)], "fixture_certificate_sign_failed")
    for path in directory.iterdir():
        if path.is_file():
            path.chmod(0o600)


class FixtureHandler(BaseHTTPRequestHandler):
    def log_message(self, *arguments):
        pass

    def do_GET(self):
        self.probe()

    def do_POST(self):
        self.probe()

    def probe(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path.startswith("/api/v1/does-not-exist"):
            response, status = {"error": "not found"}, 404
        else:
            response, status = {
                "path": self.path, "length": len(body), "sha256": hashlib.sha256(body).hexdigest(),
                "host": self.headers.get("Host"), "xff": self.headers.get("X-Forwarded-For"),
                "forwarded": self.headers.get("Forwarded"), "realip": self.headers.get("X-Real-IP"),
                "auth": self.headers.get("Authorization"), "wechat_serial": self.headers.get("Wechatpay-Serial"),
                "wechat_signature": self.headers.get("Wechatpay-Signature"), "origin": self.headers.get("Origin"),
            }, 200
        data = json.dumps(response).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class Containers:
    def __init__(self, directory, run):
        self.directory, self.run, self.identities = directory, run, []

    def create(self, suffix, config, arguments):
        identity = command([
            "docker", "create", "--name", "xui-proxy-" + self.run + "-" + suffix,
            "--label", LABEL + "=" + self.run, "--network", "host", "--entrypoint", "nginx",
            "--mount", "type=bind,src=" + str(config) + ",dst=/etc/nginx/conf.d/default.conf,readonly",
            "--mount", "type=bind,src=" + str(self.directory / "public") + ",dst=/var/www/xui/current,readonly",
            "--mount", "type=bind,src=" + str(self.directory) + ",dst=/fixture",
            IMAGE, *arguments,
        ], "fixture_container_create_failed").decode().strip()
        if not re.fullmatch(r"[0-9a-f]{64}", identity):
            raise CheckFailure("invalid_fixture_container_identity")
        self.identities.append(identity)
        return identity

    def cleanup(self):
        failed = False
        for identity in reversed(self.identities):
            try:
                labels = json.loads(command(["docker", "inspect", "--format", "{{json .Config.Labels}}", identity],
                                            "fixture_container_inspect_failed"))
                if labels.get(LABEL) != self.run:
                    raise CheckFailure("fixture_container_ownership_mismatch")
                command(["docker", "rm", "-f", identity], "fixture_container_cleanup_failed")
            except (CheckFailure, ValueError):
                failed = True
        if failed:
            raise CheckFailure("fixture_container_cleanup_failed")


def available_port():
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    return listener


def run_checks(directory):
    certificate(directory)
    public = directory / "public"
    (public / "assets").mkdir(parents=True, mode=0o755)
    public.chmod(0o755)
    (public / "assets").chmod(0o755)
    for relative, content in {
        "index.html": "<!doctype html><title>fixture-xui-index</title>",
        "assets/app-AbCd1234.js": "window.fixtureProxy=true;",
        "assets/unhashed.js": "window.fixtureUnhashed=true;",
        ".env.local": "fixture-private-not-public", "release.json": "fixture-private-not-public",
    }.items():
        path = public / relative
        path.write_text(content)
        path.chmod(0o644)
    # Relative target exists inside both the host and container static mount;
    # this must fail because of disable_symlinks, not because a host-only
    # absolute target would be missing inside the container.
    (public / "assets" / "linked-AbCd1234.js").symlink_to("../release.json")
    (directory / "access.log").touch(mode=0o600)
    api = ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler)
    api.daemon_threads = True
    api_thread = threading.Thread(target=api.serve_forever, daemon=True)
    api_thread.start()
    api_open = True
    http_listener, tls_listener = available_port(), available_port()
    http_port, tls_port = http_listener.getsockname()[1], tls_listener.getsockname()[1]
    template = (ROOT / "deploy/nginx/xui.conf.example").read_text()
    # Syntax-check the original IPv4/IPv6 listeners before adapting runtime
    # listeners. nginx -t does not bind them, so existing host services stay up.
    config = template.replace("server 127.0.0.1:8080;", "server 127.0.0.1:" + str(api.server_port) + ";")
    config = config.replace("/etc/letsencrypt/live/" + HOST + "/fullchain.pem", "/fixture/server.crt")
    config = config.replace("/etc/letsencrypt/live/" + HOST + "/privkey.pem", "/fixture/server.key")
    config = config.replace("/var/log/nginx/xui-access.log", "/fixture/access.log")
    syntax = directory / "syntax.conf"
    syntax.write_text(config)
    # Host networking is Linux-only; each actual listener is literal loopback.
    for line in ["listen [::]:80 default_server;", "listen [::]:80;",
                 "listen [::]:443 ssl default_server;", "listen [::]:443 ssl;"]:
        config = config.replace(line, "")
    for old, new in [("80 default_server", str(http_port) + " default_server"),
                     ("80", str(http_port)), ("443 ssl default_server", str(tls_port) + " ssl default_server"),
                     ("443 ssl", str(tls_port) + " ssl")]:
        config = config.replace("listen " + old + ";", "listen 127.0.0.1:" + new + ";")
    config = config.replace(HOST + ":443", HOST + ":" + str(tls_port))
    runtime_config = directory / "runtime.conf"
    runtime_config.write_text(config)
    containers = Containers(directory, uuid.uuid4().hex)
    results = []

    def expect(name, condition):
        if not condition:
            raise CheckFailure(name)
        results.append(name)

    context = ssl.create_default_context(cafile=str(directory / "ca.crt"))

    def connection():
        client = http.client.HTTPSConnection(HOST, tls_port, context=context, timeout=5)
        # Keep the certificate hostname/SNI and CA verification intact while
        # connecting to a private address instead of resolving a public domain.
        client._create_connection = lambda address, timeout, *args, **kwargs: socket.create_connection(("127.0.0.1", tls_port), timeout)
        return client

    def request(path, method="GET", body=None, headers=None):
        client = connection()
        try:
            client.request(method, path, body=body, headers=headers or {})
            response = client.getresponse()
            return response.status, {key.lower(): value for key, value in response.getheaders()}, response.read()
        finally:
            client.close()

    try:
        checked = containers.create("syntax", syntax, ["-t"])
        command(["docker", "start", "-a", checked], "nginx_configuration_invalid")
        exit_code = command(["docker", "inspect", "--format", "{{.State.ExitCode}}", checked],
                            "nginx_configuration_status_unavailable")
        expect("original_ipv4_ipv6_template_syntax", exit_code.strip() == b"0")
        http_listener.close()
        tls_listener.close()
        active = containers.create("serve", runtime_config, ["-g", "daemon off;"])
        command(["docker", "start", active], "nginx_fixture_start_failed")
        deadline = time.monotonic() + 8
        while True:
            try:
                status, headers, data = request("/")
                break
            except (OSError, http.client.HTTPException):
                if time.monotonic() >= deadline:
                    raise CheckFailure("nginx_fixture_not_ready") from None
                time.sleep(0.1)
        expect("root_html_no_store", status == 200 and b"fixture-xui-index" in data and headers.get("cache-control") == "no-store")
        expect("inherited_tls_security_headers", headers.get("strict-transport-security") == "max-age=31536000" and headers.get("x-frame-options") == "DENY" and headers.get("referrer-policy") == "no-referrer")
        status, headers, data = request("/assets/app-AbCd1234.js")
        expect("hash_asset_immutable", status == 200 and "immutable" in headers.get("cache-control", "") and headers.get("strict-transport-security") == "max-age=31536000")
        status, headers, data = request("/assets/unhashed.js")
        expect("unhashed_asset_no_store", status == 200 and headers.get("cache-control") == "no-store")
        status, headers, data = request("/assets/linked-AbCd1234.js")
        expect("asset_symlink_rejected", status >= 400 and b"fixture-private-not-public" not in data)
        for path in ["/readyz", "/healthz", "/.env.local", "/release.json", "/srv/xui/current/bin/xui-control",
                     "/nginx_status", "/api/v2/config", "/agent/v2/enroll", "/unknown", "/assets/missing.js"]:
            status, headers, data = request(path)
            expect("deny_" + path, status == 404 and b"fixture-xui-index" not in data)
        status, headers, data = request("/api/v1/does-not-exist")
        expect("api_404_not_spa_html", status == 404 and json.loads(data) == {"error": "not found"})
        status, headers, data = request("/agent/v1/desired-state")
        expect("agent_path_preserved", status == 200 and json.loads(data)["path"] == "/agent/v1/desired-state")
        marker = "fixture-never-log-" + uuid.uuid4().hex
        path = "/api/v1/subscriptions/" + marker + "?format=uri"
        status, headers, data = request(path, headers={"User-Agent": marker, "Referer": "https://example.test/" + marker})
        expect("subscription_path_query_preserved", status == 200 and json.loads(data)["path"] == path and headers.get("cache-control") == "no-store")
        path = "/api/v1/payments/notify/epay?pid=123&sign=" + marker + "&trade_no=fixture"
        status, headers, data = request(path)
        expect("epay_get_query_preserved", status == 200 and json.loads(data)["path"] == path)
        payload = b'{"probe":"exact-body-unchanged","amount":"1.00"}'
        status, headers, data = request("/api/v1/payments/notify/wechat", "POST", payload, {
            "Content-Type": "application/json", "Wechatpay-Serial": "fixture-serial", "Wechatpay-Signature": marker,
            "Authorization": "Bearer " + marker, "Origin": "https://" + HOST,
            "X-Forwarded-For": "198.51.100.77", "X-Real-IP": "198.51.100.77", "Forwarded": "for=198.51.100.77",
        })
        probe = json.loads(data)
        expect("wechat_body_auth_signature_preserved", status == 200 and probe["sha256"] == hashlib.sha256(payload).hexdigest() and probe["wechat_serial"] == "fixture-serial" and probe["wechat_signature"] == marker and probe["auth"] == "Bearer " + marker and probe["origin"] == "https://" + HOST)
        expect("edge_overwrites_forged_identity", probe["xff"] == "127.0.0.1" and probe["forwarded"] is None and probe["realip"] is None and probe["host"] == HOST)
        for path, size in [("/api/v1/orders", 3 << 20), ("/api/v1/admin/statements/native", 13 << 20), ("/api/v1/payments/notify/wechat", 65537)]:
            client = connection()
            try:
                client.putrequest("POST", path)
                client.putheader("Content-Length", str(size))
                client.endheaders()
                response = client.getresponse()
                response.read()
                expect("body_limit_" + path, response.status == 413)
            finally:
                client.close()
        body = b"x" * (3 << 20)
        status, headers, data = request("/api/v1/admin/statements/native", "POST", body, {"Content-Type": "application/json"})
        expect("native_upload_limit_override", status == 200 and json.loads(data)["length"] == len(body))
        client = http.client.HTTPConnection("127.0.0.1", http_port, timeout=3)
        try:
            client.request("GET", "/?sign=" + marker, headers={"Host": HOST})
            response = client.getresponse()
            response.read()
            expect("fixed_https_redirect", response.status == 308 and response.getheader("Location") == "https://" + HOST + "/?sign=" + marker)
        finally:
            client.close()
        client = connection()
        try:
            client.request("GET", "/", headers={"Host": "unknown.example.test"})
            response = client.getresponse()
            response.read()
            expect("unknown_host_rejected", response.status >= 400)
        except (OSError, http.client.HTTPException):
            expect("unknown_host_rejected", True)
        finally:
            client.close()
        with socket.create_connection(("127.0.0.1", tls_port), 3) as raw:
            try:
                context.wrap_socket(raw, server_hostname="unknown.example.test")
            except ssl.SSLError as error:
                expect("unknown_sni_server_rejected", error.reason == "TLSV1_UNRECOGNIZED_NAME")
            else:
                raise CheckFailure("unknown_sni_accepted")
        with socket.create_connection(("127.0.0.1", tls_port), 3) as raw:
            try:
                ssl.create_default_context().wrap_socket(raw, server_hostname=HOST)
            except ssl.SSLCertVerificationError:
                expect("untrusted_certificate_rejected", True)
            else:
                raise CheckFailure("untrusted_certificate_accepted")
        api.shutdown()
        api.server_close()
        api_thread.join(timeout=5)
        api_open = False
        status, headers, data = request("/api/v1/subscriptions/" + marker + "?format=uri")
        expect("upstream_failure_real_502", status == 502 and marker.encode() not in data)
        built_in = command(["docker", "logs", active], "nginx_logs_unavailable", combined=True)
        expect("builtin_logs_no_request_secret", marker.encode() not in built_in)
        logs = (directory / "access.log").read_text()
        expect("access_logs_no_credentials", marker not in logs and "198.51.100.77" not in logs and "sign=" not in logs and "/api/v1/subscriptions/" not in logs)
        entries = [json.loads(line) for line in logs.splitlines()]
        expect("access_logs_only_safe_fields", entries and all(set(entry) == {"time", "peer", "route", "method", "status", "bytes", "seconds"} for entry in entries) and {"subscription", "payment_callback", "agent", "api", "asset", "root"} <= {entry["route"] for entry in entries})
        return {"ok": True, "checks": len(results), "nginx_image": IMAGE,
                "tls": "verified synthetic CA, hostname and SNI", "public_listener": False,
                "result_names": results}
    finally:
        http_listener.close()
        tls_listener.close()
        try:
            containers.cleanup()
        finally:
            if api_open:
                api.shutdown()
                api.server_close()
                api_thread.join(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pull", action="store_true", help="explicitly pull the pinned Nginx image before testing")
    args = parser.parse_args()
    try:
        if not sys.platform.startswith("linux"):
            raise CheckFailure("linux_host_network_required")
        if not shutil.which("docker") or not shutil.which("openssl"):
            raise CheckFailure("docker_and_openssl_required")
        if args.pull:
            command(["docker", "pull", IMAGE], "pinned_image_pull_failed", timeout=60)
        command(["docker", "image", "inspect", IMAGE], "pinned_image_missing_run_with_pull")
        with tempfile.TemporaryDirectory(prefix="xui-proxy-test-") as temporary:
            result = run_checks(Path(temporary))
        print(json.dumps(result, indent=2))
        return 0
    except (CheckFailure, OSError, ValueError, http.client.HTTPException) as error:
        code = str(error) if isinstance(error, CheckFailure) else "proxy_fixture_unavailable"
        print(json.dumps({"ok": False, "error": code}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
