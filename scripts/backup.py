#!/usr/bin/env python3
"""Private PostgreSQL custom archives and non-destructive restore verification.

All database commands run inside the configured Compose PostgreSQL service.
Passwords remain in the container environment; no connection URI is accepted or
printed. A restore always creates a new database and never uses --clean.
"""
from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
NAME = re.compile(r"^[a-z][a-z0-9_]{0,62}$")
SNAPSHOT = re.compile(r"^[0-9A-Fa-f]+-[0-9A-Fa-f]+-[0-9A-Fa-f]+$")
FORMAT = "xui-postgres-backup-v2"
LEGACY_FORMAT = "xui-postgres-backup-v1"
FINGERPRINT_ALGORITHM = "sorted-sha256-rows-v2"
FINGERPRINT_BATCH_ROWS = 4096
FINGERPRINT_END = "xui_fingerprint_batch_end"
ROW_FINGERPRINT = re.compile(r"^[0-9a-f]{64}$")
MAX_MANIFEST_BYTES = 4 << 20


class BackupError(Exception):
    """Finite operational error; subprocess output is deliberately private."""


def database_name(value: str) -> str:
    if not NAME.fullmatch(value):
        raise BackupError("invalid_database_name")
    return value


def identifier(value: str) -> str:
    return '"' + value.replace('"', '""') + '"'


def literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def private_parent(path: Path) -> Path:
    path = Path(os.path.abspath(path))
    for item in (path, *path.parents):
        if item.is_symlink():
            raise BackupError("symlink_path_rejected")
    try:
        info = path.stat()
    except FileNotFoundError:
        raise BackupError("private_parent_directory_required") from None
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise BackupError("private_parent_directory_required")
    return path


def private_file(path: Path):
    private_parent(path.parent)
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    except OSError:
        raise BackupError("private_regular_file_required") from None
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_nlink != 1:
        os.close(fd)
        raise BackupError("private_regular_file_required")
    return os.fdopen(fd, "rb")


def sync_directory(directory: Path):
    fd = os.open(directory, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def publish_exclusive(temporary: Path, destination: Path):
    # A hard link publishes a complete synced file atomically without replacing
    # any existing path, including a symlink. Both files are in one directory.
    try:
        os.link(temporary, destination, follow_symlinks=False)
    except FileExistsError:
        raise BackupError("backup_destination_exists") from None
    temporary.unlink()
    sync_directory(destination.parent)


class Backend:
    def __init__(self, compose_file: Path, env_file: Path | None, service: str, user: str, timeout: int):
        if not NAME.fullmatch(service) or not NAME.fullmatch(user) or not 5 <= timeout <= 7200:
            raise BackupError("invalid_backend_configuration")
        self.base = ["docker", "compose", "-f", str(compose_file)]
        if env_file is not None:
            self.base += ["--env-file", str(env_file)]
        self.base += ["exec", "-T", service]
        self.user, self.timeout = user, timeout

    def command(self, program: str, *args: str) -> list[str]:
        # Only this fixed shell program is evaluated. Arguments are passed via
        # "$@", never interpolated as shell code. Credentials never leave env.
        return self.base + ["sh", "-c", 'export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec timeout "$@"',
                            "xui-backup", str(self.timeout), program, *args]

    def run(self, program: str, *args: str, input_data: bytes | None = None, stdin=None, stdout=None) -> bytes:
        try:
            result = subprocess.run(self.command(program, *args), input=input_data, stdin=stdin,
                                    stdout=stdout if stdout is not None else subprocess.PIPE,
                                    stderr=subprocess.PIPE, cwd=ROOT, timeout=self.timeout + 10, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise BackupError("database_command_unavailable_or_timed_out") from None
        if result.returncode:
            raise BackupError("database_command_failed")
        return result.stdout or b""

    def sql(self, database: str, statement: str) -> bytes:
        return self.run("psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", self.user,
                        "-d", database_name(database), input_data=statement.encode())

    def create(self, database: str):
        self.run("createdb", "-U", self.user, "--maintenance-db=postgres", "--template=template0", database_name(database))

    def drop_owned(self, database: str):
        self.run("dropdb", "-U", self.user, "--maintenance-db=postgres", "--force", database_name(database))


class SnapshotSession:
    def __init__(self, backend: Backend, database: str):
        self.backend = backend
        self.process = subprocess.Popen(backend.command("psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1",
                                                       "-U", backend.user, "-d", database_name(database)),
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                        text=True, cwd=ROOT)
        try:
            self.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SET TIME ZONE 'UTC'; SET LOCAL work_mem='4MB'; SET LOCAL max_parallel_workers_per_gather=0; SELECT 1;")
        except Exception:
            self.close()
            raise

    def query(self, statement: str) -> str:
        assert self.process.stdin is not None and self.process.stdout is not None
        try:
            self.process.stdin.write(statement + "\n")
            self.process.stdin.flush()
            line = self.process.stdout.readline(MAX_MANIFEST_BYTES + 2)
        except OSError:
            raise BackupError("snapshot_query_failed") from None
        if not line or not line.endswith("\n") or len(line) > MAX_MANIFEST_BYTES + 1:
            raise BackupError("snapshot_query_failed")
        return line.rstrip("\n")

    def row_fingerprint(self, table: str) -> str:
        # Keep one exported transaction/snapshot. PostgreSQL sorts fixed-size
        # row digests with bounded work_mem (spilling to temporary disk); it
        # never materializes an entire table into a single text value. FETCH
        # bounds each batch while Python hashes one digest at a time.
        self.query("DECLARE xui_fingerprint NO SCROLL CURSOR FOR SELECT fingerprint FROM "
                   "(SELECT encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex') AS fingerprint "
                   "FROM " + table + " r) leaf_rows ORDER BY fingerprint COLLATE \"C\"; SELECT 1;")
        digest = hashlib.sha256((FINGERPRINT_ALGORITHM + "\n").encode("ascii"))
        assert self.process.stdin is not None and self.process.stdout is not None
        while True:
            try:
                self.process.stdin.write("FETCH FORWARD " + str(FINGERPRINT_BATCH_ROWS)
                                         + " FROM xui_fingerprint; SELECT '" + FINGERPRINT_END + "';\n")
                self.process.stdin.flush()
                count = 0
                while True:
                    line = self.process.stdout.readline(128)
                    if not line or not line.endswith("\n"):
                        raise BackupError("snapshot_fingerprint_failed")
                    value = line[:-1]
                    if value == FINGERPRINT_END:
                        break
                    if not ROW_FINGERPRINT.fullmatch(value) or count >= FINGERPRINT_BATCH_ROWS:
                        raise BackupError("snapshot_fingerprint_failed")
                    digest.update((value + "\n").encode("ascii"))
                    count += 1
            except OSError:
                raise BackupError("snapshot_fingerprint_failed") from None
            if count < FINGERPRINT_BATCH_ROWS:
                break
        self.query("CLOSE xui_fingerprint; SELECT 1;")
        return digest.hexdigest()

    def json(self, statement: str):
        try:
            return json.loads(self.query(statement))
        except (ValueError, TypeError):
            raise BackupError("snapshot_metadata_invalid") from None

    def close(self):
        if self.process.stdin:
            with contextlib.suppress(OSError):
                self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)
        if self.process.stdout:
            self.process.stdout.close()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


def snapshot_evidence(session: SnapshotSession) -> dict:
    tables = session.json("SELECT COALESCE(json_agg(json_build_array(n.nspname,c.relname) ORDER BY n.nspname,c.relname),'[]'::json) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p') AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema';")
    counts = {}
    financial = {}
    for schema, name in tables:
        table = identifier(schema) + "." + identifier(name)
        key = schema + "." + name
        counts[key] = int(session.query("SELECT count(*) FROM " + table + ";"))
        # Aggregate immutable row fingerprints instead of putting account rows,
        # password hashes, credentials or financial payloads into the manifest.
        if name in {"schema_migrations", "users", "sessions", "staff_mfa", "staff_mfa_recovery",
                    "orders", "payment_events", "payment_intents", "payment_refunds",
                    "entitlement_periods", "usage_batches", "usage_items", "merchant_statements",
                    "merchant_statement_financials", "fund_ledger_batches", "fund_ledger_reconciliations"}:
            financial[key] = session.row_fingerprint(table)
    objects = session.json("""SELECT json_build_object(
      'constraints',COALESCE((SELECT json_agg(json_build_array(n.nspname,c.relname,k.conname,k.contype,pg_get_constraintdef(k.oid)) ORDER BY n.nspname,c.relname,k.conname) FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema'),'[]'::json),
      'indexes',COALESCE((SELECT json_agg(json_build_array(schemaname,tablename,indexname,indexdef) ORDER BY schemaname,tablename,indexname) FROM pg_indexes WHERE schemaname !~ '^pg_' AND schemaname<>'information_schema'),'[]'::json),
      'triggers',COALESCE((SELECT json_agg(json_build_array(n.nspname,c.relname,t.tgname,pg_get_triggerdef(t.oid)) ORDER BY n.nspname,c.relname,t.tgname) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema'),'[]'::json));""")
    sums = {}
    aggregate_fields = {"orders": ["price_minor"], "payment_events": ["amount_minor"],
                        "payment_refunds": ["amount_minor"], "entitlement_periods": ["quota_bytes", "used_bytes"],
                        "usage_items": ["uplink_bytes", "downlink_bytes"], "merchant_statements": ["total_minor"]}
    for schema, name in tables:
        if name in aggregate_fields:
            available = session.json("SELECT COALESCE(json_agg(column_name),'[]'::json) FROM information_schema.columns WHERE table_schema=" + literal(schema) + " AND table_name=" + literal(name) + ";")
            for field in aggregate_fields[name]:
                if field in available:
                    sums[schema + "." + name + "." + field] = session.query("SELECT COALESCE(sum(" + identifier(field) + "),0)::text FROM " + identifier(schema) + "." + identifier(name) + ";")
    return {"fingerprint_algorithm": FINGERPRINT_ALGORITHM, "table_counts": counts,
            "financial_fingerprints": financial, "critical_sums": sums, **objects}


def archive_digest(stream) -> tuple[str, int]:
    digest, size = hashlib.sha256(), 0
    while chunk := stream.read(1 << 20):
        digest.update(chunk)
        size += len(chunk)
    return digest.hexdigest(), size


def manifest_path(archive: Path) -> Path:
    return archive.with_name(archive.name + ".manifest.json")


def verify(archive: Path) -> dict:
    archive = Path(os.path.abspath(archive))
    with private_file(manifest_path(archive)) as stream:
        raw = stream.read(MAX_MANIFEST_BYTES + 1)
    if len(raw) > MAX_MANIFEST_BYTES:
        raise BackupError("manifest_too_large")
    try:
        manifest = json.loads(raw)
    except (ValueError, UnicodeDecodeError):
        raise BackupError("invalid_manifest") from None
    if not isinstance(manifest, dict) or manifest.get("format") not in (FORMAT, LEGACY_FORMAT) or manifest.get("archive_name") != archive.name or not isinstance(manifest.get("evidence"), dict):
        raise BackupError("invalid_manifest")
    if manifest["format"] == FORMAT and manifest["evidence"].get("fingerprint_algorithm") != FINGERPRINT_ALGORITHM:
        raise BackupError("invalid_manifest")
    with private_file(archive) as stream:
        if stream.read(5) != b"PGDMP":
            raise BackupError("invalid_archive_format")
        stream.seek(0)
        digest, size = archive_digest(stream)
    if digest != manifest.get("sha256") or size != manifest.get("bytes"):
        raise BackupError("archive_integrity_mismatch")
    return manifest


def backup(backend: Backend, database: str, archive: Path) -> dict:
    database_name(database)
    archive = Path(os.path.abspath(archive))
    private_parent(archive.parent)
    target_manifest = manifest_path(archive)
    if os.path.lexists(archive) or os.path.lexists(target_manifest):
        raise BackupError("backup_destination_exists")
    temporary = None
    manifest_temporary = None
    published = False
    manifest_published = False
    try:
        with SnapshotSession(backend, database) as session:
            token = session.query("SELECT pg_export_snapshot();")
            if not SNAPSHOT.fullmatch(token):
                raise BackupError("snapshot_metadata_invalid")
            evidence = snapshot_evidence(session)
            fd, name = tempfile.mkstemp(prefix=".xui-dump-", dir=archive.parent)
            temporary = Path(name)
            with os.fdopen(fd, "w+b") as stream:
                backend.run("pg_dump", "-U", backend.user, "-d", database, "--format=custom", "--no-owner", "--no-acl", "--snapshot=" + token, stdout=stream)
                stream.flush()
                os.fsync(stream.fileno())
                stream.seek(0)
                if stream.read(5) != b"PGDMP":
                    raise BackupError("invalid_archive_format")
                stream.seek(0)
                digest, size = archive_digest(stream)
        manifest = {"format": FORMAT, "archive_name": archive.name, "created_at_unix": int(time.time()),
                    "sha256": digest, "bytes": size, "evidence": evidence}
        raw = (json.dumps(manifest, ensure_ascii=True, sort_keys=True, separators=(",", ":")) + "\n").encode()
        if len(raw) > MAX_MANIFEST_BYTES:
            raise BackupError("manifest_too_large")
        fd, name = tempfile.mkstemp(prefix=".xui-manifest-", dir=archive.parent)
        manifest_temporary = Path(name)
        with os.fdopen(fd, "wb") as stream:
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        publish_exclusive(temporary, archive)
        temporary = None
        published = True
        # Manifest is the commit marker: an interrupted pair fails verification.
        publish_exclusive(manifest_temporary, target_manifest)
        manifest_temporary = None
        manifest_published = True
        verify(archive)
        return manifest
    except Exception:
        if manifest_published:
            with contextlib.suppress(FileNotFoundError):
                target_manifest.unlink()
        if published:
            with contextlib.suppress(FileNotFoundError):
                archive.unlink()
        raise
    finally:
        for name in (temporary, manifest_temporary):
            if name is not None:
                with contextlib.suppress(FileNotFoundError):
                    name.unlink()


def restore(backend: Backend, archive: Path, target: str, keep: bool) -> dict:
    target = database_name(target)
    manifest = verify(archive)
    if manifest["format"] != FORMAT:
        raise BackupError("legacy_backup_restore_requires_v1_tool")
    # Verify the native archive directory before creating any target database.
    with private_file(archive) as stream:
        backend.run("pg_restore", "--list", stdin=stream)
    created = False
    succeeded = False
    started = time.monotonic()
    try:
        backend.create(target)  # Fails when it already exists; no --if-not-exists.
        created = True
        with private_file(archive) as stream:
            backend.run("pg_restore", "-U", backend.user, "-d", target, "--single-transaction", "--exit-on-error", "--no-owner", "--no-acl", stdin=stream)
        with SnapshotSession(backend, target) as session:
            evidence = snapshot_evidence(session)
        if evidence != manifest["evidence"]:
            raise BackupError("restored_evidence_mismatch")
        succeeded = True
        return {"verified": True, "restored": True, "retained": keep,
                "table_count": len(evidence["table_counts"]), "archive_bytes": manifest["bytes"],
                "duration_seconds": round(time.monotonic() - started, 3), **({"database": target} if keep else {})}
    finally:
        if created and (not keep or not succeeded):
            backend.drop_owned(target)


def fixture_migrations() -> str:
    source = (ROOT / "internal/store/store.go").read_text()
    embeds = {name: filename for filename, name in re.findall(r"//go:embed ([a-z_]+\.sql)\s+var (\w+) string", source)}
    found = re.search(r"range \[\]string\{([^}]+)\}", source)
    if not found:
        raise BackupError("fixture_migration_catalog_unavailable")
    names = [name.strip() for name in found.group(1).split(",")]
    if not names or any(name not in embeds for name in names):
        raise BackupError("fixture_migration_catalog_unavailable")
    sql = "BEGIN; CREATE TABLE schema_migrations(version integer PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now());\n"
    for version, name in enumerate(names, 1):
        sql += (ROOT / "internal/store" / embeds[name]).read_text() + f"\nINSERT INTO schema_migrations(version) VALUES({version});\n"
    return sql + "COMMIT;\n"


FIXTURE_SQL = """
INSERT INTO users(id,email,password_hash,role) VALUES('backup-user','backup-fixture@example.test','not-a-login-hash','admin');
INSERT INTO sessions(token_hash,user_id,expires_at,mfa_verified_at) VALUES(repeat('a',64),'backup-user',now()+interval '1 hour',now());
INSERT INTO staff_mfa(user_id,secret_cipher,enabled_at,last_counter) VALUES('backup-user',decode('aabbcc','hex'),now(),123);
INSERT INTO staff_mfa_recovery(user_id,code_hash,used_at) VALUES('backup-user',repeat('b',64),now());
INSERT INTO plans(id,name,description,price_minor,currency,duration_days,quota_bytes) VALUES('backup-plan','Backup fixture','Synthetic recovery evidence',1900,'CNY',30,1000000);
INSERT INTO orders(id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes,status,paid_at) VALUES('backup-order','backup-user','backup-plan','Backup fixture',1900,'CNY',30,1000000,'paid',now());
INSERT INTO orders(id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes,status)
 SELECT 'backup-order-'||g,'backup-user','backup-plan','Backup fixture',1900,'CNY',30,1000000,'pending'
 FROM generate_series(1,4097) g;
INSERT INTO payment_events(provider,event_id,order_id,amount_minor,currency) VALUES('test','backup-event','backup-order',1900,'CNY');
INSERT INTO payment_intents(id,order_id,provider,status,transaction_id,paid_at) VALUES('backup-payment','backup-order','alipay','paid','backup-transaction',now());
INSERT INTO entitlement_periods(id,user_id,order_id,plan_name,starts_at,ends_at,quota_bytes,used_bytes) VALUES('backup-period','backup-user','backup-order','Backup fixture',now()-interval '1 hour',now()+interval '1 day',1000000,12345);
INSERT INTO nodes(id,name,enrollment_hash,enrollment_expires_at) VALUES('backup-node','Backup fixture','fixture-not-a-secret',now()+interval '1 hour');
INSERT INTO usage_batches(node_id,batch_id,core_instance_id,sequence,started_at,ended_at,digest,payload) VALUES('backup-node','backup-batch','backup-instance',1,now()-interval '1 minute',now(),'fixture-digest','{}');
INSERT INTO usage_items(node_id,batch_id,item_index,binding_id,user_id,entitlement_period_id,uplink_bytes,downlink_bytes) VALUES('backup-node','backup-batch',0,'fixture','backup-user','backup-period',2345,10000);
INSERT INTO audit_events(actor_id,action,resource_id) VALUES('backup-user','backup.fixture','backup-order');
"""


def self_test(backend: Backend, directory: Path) -> dict:
    directory = private_parent(directory)
    source = "xui_backup_fixture_" + secrets.token_hex(10)
    created = False
    archive = directory / ("fixture-" + secrets.token_hex(8) + ".dump")
    try:
        backend.create(source)
        created = True
        backend.sql(source, fixture_migrations() + FIXTURE_SQL)
        manifest = backup(backend, source, archive)
        result = restore(backend, archive, "xui_restore_" + secrets.token_hex(12), keep=False)
        if manifest["evidence"]["critical_sums"].get("public.usage_items.uplink_bytes") != "2345" or manifest["evidence"]["critical_sums"].get("public.entitlement_periods.used_bytes") != "12345" or manifest["evidence"]["table_counts"].get("public.staff_mfa_recovery") != 1 or manifest["evidence"]["table_counts"].get("public.orders", 0) <= FINGERPRINT_BATCH_ROWS:
            raise BackupError("fixture_evidence_invalid")
        return {"self_test": True, "full_migrations": True, "synthetic_financial_and_usage_rows": True,
                "synthetic_mfa_security_state": True, "temporary_databases_removed": True, **result}
    finally:
        if created:
            backend.drop_owned(source)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--compose-file", type=Path, default=ROOT / "compose.yaml")
    parser.add_argument("--env-file", type=Path, default=ROOT / ".env.local")
    parser.add_argument("--service", default="db")
    parser.add_argument("--user", default="xui")
    parser.add_argument("--timeout", type=int, default=600)
    sub = parser.add_subparsers(dest="action", required=True)
    p = sub.add_parser("backup"); p.add_argument("--database", required=True); p.add_argument("--output", required=True, type=Path)
    p = sub.add_parser("verify"); p.add_argument("--input", required=True, type=Path)
    p = sub.add_parser("restore-drill"); p.add_argument("--input", required=True, type=Path)
    p = sub.add_parser("restore-new"); p.add_argument("--input", required=True, type=Path); p.add_argument("--database", required=True)
    p = sub.add_parser("self-test"); p.add_argument("--directory", required=True, type=Path)
    args = parser.parse_args(argv)
    try:
        if args.action == "verify":
            manifest = verify(args.input)
            result = {"verified": True, "archive_bytes": manifest["bytes"], "table_count": len(manifest["evidence"]["table_counts"])}
        else:
            backend = Backend(args.compose_file, args.env_file if args.env_file.exists() else None, args.service, args.user, args.timeout)
            if args.action == "backup":
                manifest = backup(backend, args.database, args.output)
                result = {"backed_up": True, "archive_bytes": manifest["bytes"], "table_count": len(manifest["evidence"]["table_counts"])}
            elif args.action == "self-test":
                result = self_test(backend, args.directory)
            else:
                target = args.database if args.action == "restore-new" else "xui_restore_" + secrets.token_hex(12)
                result = restore(backend, args.input, target, keep=args.action == "restore-new")
        print(json.dumps(result, sort_keys=True))
        return 0
    except (BackupError, OSError, ValueError, KeyError, TypeError) as error:
        # Never print subprocess stderr, connection details, archive/SQL data or
        # exception values containing paths/credentials from the environment.
        code = str(error) if isinstance(error, BackupError) else "local_configuration_or_io_error"
        print(json.dumps({"ok": False, "error": code}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
