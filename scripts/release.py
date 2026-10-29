#!/usr/bin/env python3
"""Private local releases with identity-checked readiness and binary rollback.

This is an operator tool, not a download/update endpoint. It never rolls back a
database migration and never starts a shell. CLI errors contain finite codes.
"""

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


FORMAT = "xui-local-release-v1"
EPOCH = "staff-mfa-v1"
MAX_FILES = 4096
MAX_FILE_BYTES = 128 << 20
MAX_TOTAL_BYTES = 512 << 20
ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
HEX = re.compile(r"^[0-9a-f]{64}$")


class ReleaseError(Exception):
    def __init__(self, code):
        super().__init__(code)
        self.code = code


def require(condition, code):
    if not condition:
        raise ReleaseError(code)


def release_id(value):
    require(isinstance(value, str) and ID.fullmatch(value), "invalid_release_id")
    return value


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def no_symlink_parents(path):
    for value in [*reversed(path.parents), path]:
        if value.exists() or value.is_symlink():
            require(not stat.S_ISLNK(value.lstat().st_mode), "symlink_path")


def private_directory(path, create=False):
    no_symlink_parents(path)
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid()
            and stat.S_IMODE(info.st_mode) & 0o077 == 0, "private_directory_required")


def managed_root(value, create=False):
    root = Path(os.path.abspath(value))
    private_directory(root, create)
    private_directory(root / "releases", create)
    return root


def strict_json(data):
    def pairs(values):
        result = {}
        for key, value in values:
            require(key not in result, "invalid_json")
            result[key] = value
        return result
    try:
        return json.loads(data, object_pairs_hook=pairs)
    except (ValueError, UnicodeError):
        raise ReleaseError("invalid_json") from None


def read_private_json(path, limit=1 << 20):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600
                and info.st_size <= limit, "invalid_private_json")
        with os.fdopen(fd, "rb", closefd=False) as source:
            data = source.read(limit + 1)
        require(len(data) <= limit, "invalid_private_json")
        return strict_json(data)
    finally:
        os.close(fd)


def atomic_json(path, value):
    temporary = path.parent / (".json-" + uuid.uuid4().hex)
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as output:
            output.write((json.dumps(value, sort_keys=True, indent=2) + "\n").encode())
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        if temporary.exists():
            temporary.unlink()


@contextlib.contextmanager
def operation_lock(root):
    fd = os.open(root / ".release.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        info = os.fstat(fd)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid()
                and info.st_nlink == 1 and stat.S_IMODE(info.st_mode) == 0o600,
                "invalid_lock")
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ReleaseError("release_operation_busy") from None
        yield
    finally:
        os.close(fd)


def safe_artifact_path(value):
    require(isinstance(value, str) and len(value) <= 1024 and "\\" not in value, "invalid_artifact_path")
    path = PurePosixPath(value)
    require(len(path.parts) <= 16 and not path.is_absolute() and str(path) == value
            and all(part not in {"", ".", ".."} and not part.startswith(".") for part in path.parts),
            "invalid_artifact_path")
    require(value in {"bin/xui-control", "bin/xui-agent"} or value.startswith("web/"),
            "invalid_artifact_path")
    return path


def file_digest(path, private=False, executable=False):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        before = os.fstat(fd)
        require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1
                and before.st_size <= MAX_FILE_BYTES, "invalid_artifact")
        if private:
            require(before.st_uid == os.geteuid() and stat.S_IMODE(before.st_mode)
                    == (0o700 if executable else 0o600), "invalid_artifact_mode")
        elif executable:
            require(before.st_mode & 0o111, "executable_required")
        digest = hashlib.sha256()
        size = 0
        with os.fdopen(fd, "rb", closefd=False) as source:
            while block := source.read(1 << 20):
                size += len(block)
                require(size <= MAX_FILE_BYTES, "artifact_too_large")
                digest.update(block)
        after = os.fstat(fd)
        require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
                == (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
                and size == before.st_size, "artifact_changed")
        return digest.hexdigest(), size
    finally:
        os.close(fd)


def source_files(control, agent, web):
    paths = [("bin/xui-control", Path(control), True), ("bin/xui-agent", Path(agent), True)]
    web = Path(os.path.abspath(web))
    no_symlink_parents(web)
    require(web.is_dir(), "web_directory_required")
    tree_entries = 0
    for directory, directories, files in os.walk(web, followlinks=False):
        tree_entries += len(directories) + len(files)
        require(tree_entries <= MAX_FILES * 2, "release_too_large")
        for name in directories:
            path = Path(directory) / name
            require(not path.is_symlink() and not name.startswith("."), "invalid_web_tree")
            safe_artifact_path("web/" + path.relative_to(web).as_posix())
        for name in files:
            path = Path(directory) / name
            relative = "web/" + path.relative_to(web).as_posix()
            safe_artifact_path(relative)
            paths.append((relative, path, False))
            require(len(paths) <= MAX_FILES, "release_too_large")
    require(3 <= len(paths) <= MAX_FILES and any(name == "web/index.html" for name, _, _ in paths),
            "incomplete_artifacts")
    return sorted(paths)


def create_artifact_parents(root, parent):
    current = root
    for part in parent.relative_to(root).parts:
        current = current / part
        current.mkdir(mode=0o700, exist_ok=True)
        private_directory(current)


def pack(root_value, identity, control, agent, web, epoch=EPOCH,
         schema_version=16, schema_min=16, schema_max=16):
    root = managed_root(root_value, True)
    identity = release_id(identity)
    require(isinstance(epoch, str) and ID.fullmatch(epoch), "invalid_epoch")
    require(all(type(v) is int and 1 <= v <= 10000 for v in [schema_version, schema_min, schema_max])
            and schema_min <= schema_version <= schema_max, "invalid_schema_range")
    with operation_lock(root):
        destination = root / "releases" / identity
        require(not destination.exists() and not destination.is_symlink(), "release_exists")
        temporary = root / "releases" / (".stage-" + uuid.uuid4().hex)
        temporary.mkdir(mode=0o700)
        try:
            artifacts = []
            total = 0
            for relative, source, executable in source_files(control, agent, web):
                no_symlink_parents(Path(os.path.abspath(source)))
                digest, size = file_digest(source, executable=executable)
                total += size
                require(total <= MAX_TOTAL_BYTES, "release_too_large")
                target = temporary / relative
                create_artifact_parents(temporary, target.parent)
                # Source is opened without following links; destination is new.
                source_fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
                target_fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                                    0o700 if executable else 0o600)
                with os.fdopen(source_fd, "rb") as incoming, os.fdopen(target_fd, "wb") as output:
                    copied = 0
                    while block := incoming.read(min(1 << 20, MAX_FILE_BYTES + 1 - copied)):
                        copied += len(block)
                        require(copied <= MAX_FILE_BYTES, "artifact_too_large")
                        output.write(block)
                    output.flush()
                    os.fsync(output.fileno())
                require(file_digest(target, private=True, executable=executable) == (digest, size),
                        "artifact_changed")
                artifacts.append({"path": relative, "sha256": digest, "size": size,
                                  "executable": executable})
            manifest = {"format": FORMAT, "release_id": identity, "compatibility_epoch": epoch,
                        "schema_version": schema_version, "schema_min": schema_min,
                        "schema_max": schema_max, "artifacts": artifacts}
            atomic_json(temporary / "release.json", manifest)
            for directory, _, _ in os.walk(temporary, topdown=False):
                sync_directory(directory)
            os.rename(temporary, destination)
            sync_directory(destination.parent)
            verify(root, identity)
            return manifest
        finally:
            if temporary.exists():
                shutil.rmtree(temporary)


def verify(root_value, identity):
    root = managed_root(root_value)
    identity = release_id(identity)
    release = root / "releases" / identity
    private_directory(release)
    manifest = read_private_json(release / "release.json")
    require(type(manifest) is dict and set(manifest) == {
        "format", "release_id", "compatibility_epoch", "schema_version", "schema_min", "schema_max", "artifacts"},
        "invalid_manifest")
    require(manifest["format"] == FORMAT and manifest["release_id"] == identity
            and isinstance(manifest["compatibility_epoch"], str)
            and ID.fullmatch(manifest["compatibility_epoch"]), "invalid_manifest")
    require(all(type(manifest[k]) is int and 1 <= manifest[k] <= 10000
                for k in ["schema_version", "schema_min", "schema_max"])
            and manifest["schema_min"] <= manifest["schema_version"] <= manifest["schema_max"],
            "invalid_schema_range")
    artifacts = manifest["artifacts"]
    require(type(artifacts) is list and 3 <= len(artifacts) <= MAX_FILES, "invalid_manifest")
    expected = {"release.json"}
    total = 0
    for artifact in artifacts:
        require(type(artifact) is dict and set(artifact) == {"path", "sha256", "size", "executable"},
                "invalid_manifest")
        relative = str(safe_artifact_path(artifact["path"]))
        executable = relative in {"bin/xui-control", "bin/xui-agent"}
        require(relative not in expected and isinstance(artifact["sha256"], str)
                and HEX.fullmatch(artifact["sha256"]) and type(artifact["size"]) is int
                and 0 <= artifact["size"] <= MAX_FILE_BYTES
                and type(artifact["executable"]) is bool and artifact["executable"] == executable,
                "invalid_manifest")
        expected.add(relative)
        total += artifact["size"]
        require(total <= MAX_TOTAL_BYTES, "release_too_large")
        no_symlink_parents(release / relative)
        digest, size = file_digest(release / relative, private=True, executable=executable)
        require((digest, size) == (artifact["sha256"], artifact["size"]), "artifact_digest_mismatch")
    require({"bin/xui-control", "bin/xui-agent", "web/index.html"}.issubset(expected), "incomplete_artifacts")
    actual = set()
    for directory, directories, files in os.walk(release, followlinks=False):
        private_directory(Path(directory))
        for name in directories:
            require(not (Path(directory) / name).is_symlink(), "symlink_path")
        for name in files:
            actual.add((Path(directory) / name).relative_to(release).as_posix())
    require(actual == expected, "unlisted_artifact")
    return manifest


def current_release(root, verify_target=True):
    path = root / "current"
    if not path.exists() and not path.is_symlink():
        return None
    require(path.is_symlink() and path.lstat().st_uid == os.geteuid(), "invalid_current_link")
    value = os.readlink(path)
    require(value.startswith("releases/") and value.count("/") == 1, "invalid_current_link")
    identity = release_id(value.split("/", 1)[1])
    if verify_target:
        verify(root, identity)
    return identity


def switch(root, identity):
    if identity is None:
        (root / "current").unlink(missing_ok=True)
    else:
        temporary = root / (".current-" + uuid.uuid4().hex)
        try:
            os.symlink("releases/" + release_id(identity), temporary)
            os.replace(temporary, root / "current")
        finally:
            temporary.unlink(missing_ok=True)
    sync_directory(root)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ReleaseError("readiness_redirect")


def ready_url(value):
    try:
        parsed = urllib.parse.urlsplit(value)
        port = parsed.port
    except ValueError:
        raise ReleaseError("invalid_readiness_url") from None
    require(parsed.scheme in {"http", "https"} and parsed.hostname and not parsed.username
            and not parsed.password and not parsed.query and not parsed.fragment
            and parsed.path == "/readyz" and (port is None or 1 <= port <= 65535),
            "invalid_readiness_url")
    if parsed.scheme == "http":
        import ipaddress
        try:
            local = ipaddress.ip_address(parsed.hostname).is_loopback
        except ValueError:
            local = False
        require(local, "https_or_literal_loopback_required")
    return value


def probe(value, request_timeout=2):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    request = urllib.request.Request(ready_url(value), headers={"Accept": "application/json", "Cache-Control": "no-cache"})
    try:
        with opener.open(request, timeout=request_timeout) as response:
            require(response.status == 200, "readiness_unavailable")
            data = response.read(4097)
            require(len(data) <= 4096, "invalid_readiness")
    except ReleaseError:
        raise
    except (OSError, urllib.error.URLError):
        raise ReleaseError("readiness_unavailable") from None
    result = strict_json(data)
    require(type(result) is dict and result.get("status") == "ok"
            and isinstance(result.get("release"), str) and ID.fullmatch(result["release"])
            and type(result.get("schema_version")) is int and 1 <= result["schema_version"] <= 10000,
            "invalid_readiness")
    return result


def wait_ready(value, manifest, timeout):
    deadline = time.monotonic() + timeout
    while True:
        try:
            result = probe(value, min(2, max(.05, deadline - time.monotonic())))
            if (result["release"] == manifest["release_id"]
                    and max(manifest["schema_min"], manifest["schema_version"])
                    <= result["schema_version"] <= manifest["schema_max"]):
                return result
        except ReleaseError:
            pass
        if time.monotonic() >= deadline:
            raise ReleaseError("release_not_ready")
        time.sleep(min(.1, max(0, deadline - time.monotonic())))


def validate_restart_argv(argv):
    require(type(argv) is list and 1 <= len(argv) <= 32
            and all(type(arg) is str and arg and len(arg) <= 4096 and "\x00" not in arg for arg in argv),
            "invalid_restart_argv")


def restart(argv, root, identity):
    validate_restart_argv(argv)
    environment = os.environ.copy()
    environment["XUI_RELEASE_ROOT"] = str(root)
    environment["XUI_RELEASE_ID"] = identity or ""
    try:
        result = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=30, env=environment, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseError("restart_failed") from None
    require(result.returncode == 0, "restart_failed")


def clear_journal(root):
    (root / "pending.json").unlink()
    sync_directory(root)


def rollback(root, journal, argv, url, timeout):
    previous = journal["previous"]
    if previous is not None:
        manifest = verify(root, previous)
        require(manifest["compatibility_epoch"] == journal["epoch"], "incompatible_epoch")
        require(manifest["schema_min"] <= journal["expected_schema"] <= manifest["schema_max"],
                "rollback_schema_incompatible")
        # If the candidate is still observable, do not restart an older binary
        # against a database it declares unsupported. An unavailable process
        # falls back to the pre-switch, durably recorded schema contract.
        try:
            observed = probe(url)
        except ReleaseError:
            observed = None
        if observed is not None:
            require(manifest["schema_min"] <= observed["schema_version"] <= manifest["schema_max"],
                    "rollback_schema_incompatible")
    switch(root, previous)
    restart(argv, root, previous)
    if previous is not None:
        wait_ready(url, manifest, timeout)
    else:
        try:
            probe(url)
        except ReleaseError:
            pass
        else:
            raise ReleaseError("bootstrap_process_still_ready")
    clear_journal(root)


def activation_options(argv, value, timeout):
    require(type(timeout) in {int, float} and .1 <= timeout <= 60, "invalid_timeout")
    ready_url(value)
    validate_restart_argv(argv)


def activate(root_value, identity, argv, value, timeout=20):
    activation_options(argv, value, timeout)
    root = managed_root(root_value)
    identity = release_id(identity)
    with operation_lock(root):
        require(not (root / "pending.json").exists() and not (root / "pending.json").is_symlink(),
                "recovery_required")
        target = verify(root, identity)
        require(target["compatibility_epoch"] == EPOCH, "incompatible_epoch")
        previous = current_release(root)
        if previous == identity:
            observed = wait_ready(value, target, timeout)
            return {"state": "already_active", "release": identity, "schema_version": observed["schema_version"]}
        expected_schema = target["schema_version"]
        if previous is not None:
            old = verify(root, previous)
            require(old["compatibility_epoch"] == target["compatibility_epoch"], "incompatible_epoch")
            observed = wait_ready(value, old, timeout)
            expected_schema = max(observed["schema_version"], target["schema_version"])
            require(target["schema_min"] <= expected_schema <= target["schema_max"]
                    and old["schema_min"] <= expected_schema <= old["schema_max"], "incompatible_schema_range")
        journal = {"format": FORMAT, "target": identity, "previous": previous,
                   "epoch": target["compatibility_epoch"], "ready_url": value,
                   "expected_schema": expected_schema}
        atomic_json(root / "pending.json", journal)
        try:
            switch(root, identity)
            restart(argv, root, identity)
            observed = wait_ready(value, target, timeout)
        except ReleaseError:
            try:
                rollback(root, journal, argv, value, timeout)
            except (ReleaseError, OSError):
                raise ReleaseError("activation_failed_recovery_required") from None
            raise ReleaseError("activation_failed_rolled_back") from None
        clear_journal(root)
        return {"state": "active", "release": identity, "previous": previous,
                "schema_version": observed["schema_version"]}


def recover(root_value, argv, value, timeout=20):
    activation_options(argv, value, timeout)
    root = managed_root(root_value)
    with operation_lock(root):
        journal = read_private_json(root / "pending.json")
        require(type(journal) is dict and set(journal) == {"format", "target", "previous", "epoch", "ready_url", "expected_schema"}
                and journal["format"] == FORMAT and journal["ready_url"] == value,
                "invalid_recovery_journal")
        release_id(journal["target"])
        if journal["previous"] is not None:
            release_id(journal["previous"])
        require(journal["epoch"] == EPOCH and type(journal["expected_schema"]) is int
                and 1 <= journal["expected_schema"] <= 10000, "invalid_recovery_journal")
        # Verify either known current target without following an arbitrary link.
        current = current_release(root, verify_target=False)
        require(current in {journal["target"], journal["previous"]}, "unexpected_current_release")
        rollback(root, journal, argv, value, timeout)
        return {"state": "recovered", "release": journal["previous"], "interrupted": journal["target"]}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    pack_parser = sub.add_parser("pack", help="copy and hash explicitly built local artifacts")
    pack_parser.add_argument("--root", required=True)
    pack_parser.add_argument("--release", required=True)
    pack_parser.add_argument("--control", required=True)
    pack_parser.add_argument("--agent", required=True)
    pack_parser.add_argument("--web", required=True)
    pack_parser.add_argument("--compatibility-epoch", default=EPOCH)
    pack_parser.add_argument("--schema-version", type=int, default=16)
    pack_parser.add_argument("--schema-min", type=int, default=16)
    pack_parser.add_argument("--schema-max", type=int, default=16)
    verify_parser = sub.add_parser("verify")
    verify_parser.add_argument("--root", required=True)
    verify_parser.add_argument("--release", required=True)
    for command in ["activate", "recover"]:
        command_parser = sub.add_parser(command)
        command_parser.add_argument("--root", required=True)
        if command == "activate":
            command_parser.add_argument("--release", required=True)
        command_parser.add_argument("--restart-json", required=True, help="JSON argv, executed without a shell; no secrets")
        command_parser.add_argument("--ready-url", default="http://127.0.0.1:8080/readyz")
        command_parser.add_argument("--timeout", type=float, default=20)
    args = parser.parse_args(argv)
    try:
        if args.command == "pack":
            manifest = pack(args.root, args.release, args.control, args.agent, args.web,
                            args.compatibility_epoch, args.schema_version, args.schema_min, args.schema_max)
            result = {"state": "packed", "release": manifest["release_id"], "artifacts": len(manifest["artifacts"])}
        elif args.command == "verify":
            manifest = verify(args.root, args.release)
            result = {"state": "verified", "release": manifest["release_id"], "artifacts": len(manifest["artifacts"])}
        else:
            restart_argv = strict_json(args.restart_json)
            if args.command == "activate":
                result = activate(args.root, args.release, restart_argv, args.ready_url, args.timeout)
            else:
                result = recover(args.root, restart_argv, args.ready_url, args.timeout)
        print(json.dumps({"ok": True, **result}, sort_keys=True))
        return 0
    except ReleaseError as error:
        print(json.dumps({"ok": False, "error": error.code}, sort_keys=True), file=sys.stderr)
        return 1
    except OSError:
        print(json.dumps({"ok": False, "error": "filesystem_unavailable"}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
