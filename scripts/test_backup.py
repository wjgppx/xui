"""Fast safety tests; the separate self-test runs native PostgreSQL recovery."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("xui_backup", Path(__file__).with_name("backup.py"))
backup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(backup)


EVIDENCE = {"table_counts": {"public.orders": 2}, "financial_fingerprints": {"public.orders": "fixture"},
            "critical_sums": {"public.orders.price_minor": "3800"}, "constraints": [], "indexes": [], "triggers": []}


class FakeSession:
    def __init__(self, *args):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def query(self, query):
        return "00000003-00000012-1"


class FakeBackend:
    def __init__(self):
        self.user = "xui"
        self.created = []
        self.dropped = []
        self.restored = []
        self.existing = set()
        self.fail_dump = False
        self.fail_restore = False
        self.fail_archive_list = False

    def create(self, name):
        if name in self.existing:
            raise backup.BackupError("database_command_failed")
        self.created.append(name)

    def drop_owned(self, name):
        self.dropped.append(name)

    def run(self, program, *args, stdin=None, stdout=None):
        if program == "pg_dump":
            stdout.write(b"PGDMPsynthetic-private-archive")
            if self.fail_dump:
                raise backup.BackupError("database_command_failed")
        elif program == "pg_restore":
            if "--list" in args:
                if self.fail_archive_list:
                    raise backup.BackupError("database_command_failed")
            else:
                self.restored.append(args[args.index("-d") + 1])
            if "--list" not in args and self.fail_restore:
                raise backup.BackupError("database_command_failed")
        return b""


class BackupSafetyTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.path = self.root / "test.dump"
        self.backend = FakeBackend()

    def make_archive(self):
        with patch.object(backup, "SnapshotSession", FakeSession), patch.object(backup, "snapshot_evidence", return_value=EVIDENCE):
            return backup.backup(self.backend, "fixture_database", self.path)

    def restore(self, keep=False, evidence=EVIDENCE):
        with patch.object(backup, "SnapshotSession", FakeSession), patch.object(backup, "snapshot_evidence", return_value=evidence):
            return backup.restore(self.backend, self.path, "new_database", keep)

    def test_private_atomic_archive_and_manifest_are_self_contained(self):
        manifest = self.make_archive()
        self.assertEqual(manifest, backup.verify(self.path))
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(backup.manifest_path(self.path).stat().st_mode & 0o777, 0o600)
        self.assertEqual(manifest["sha256"], hashlib.sha256(self.path.read_bytes()).hexdigest())
        self.assertNotIn("password", json.dumps(manifest))
        self.assertEqual(sorted(item.name for item in self.root.iterdir()), ["test.dump", "test.dump.manifest.json"])

    def test_existing_or_symlink_destinations_are_never_replaced(self):
        for symlink in (False, True):
            if symlink:
                self.path.symlink_to(self.root / "missing-target")
            else:
                self.path.write_bytes(b"preserve-existing")
            with self.assertRaisesRegex(backup.BackupError, "backup_destination_exists"):
                self.make_archive()
            if not symlink:
                self.assertEqual(self.path.read_bytes(), b"preserve-existing")
            self.path.unlink()

    def test_dump_failure_does_not_publish_or_leave_partial_files(self):
        self.backend.fail_dump = True
        with self.assertRaises(backup.BackupError):
            self.make_archive()
        self.assertEqual(list(self.root.iterdir()), [])

    def test_corrupt_archive_is_rejected_before_database_creation(self):
        self.make_archive()
        with self.path.open("ab") as output:
            output.write(b"corrupt")
        with self.assertRaisesRegex(backup.BackupError, "archive_integrity_mismatch"):
            self.restore()
        self.assertEqual(self.backend.created, [])

    def test_invalid_manifest_and_nonprivate_or_linked_archives_are_rejected(self):
        self.make_archive()
        self.path.chmod(0o644)
        with self.assertRaisesRegex(backup.BackupError, "private_regular_file_required"):
            backup.verify(self.path)
        self.path.chmod(0o600)
        link = self.root / "link.dump"
        link.symlink_to(self.path)
        with self.assertRaises(backup.BackupError):
            backup.private_file(link)
        link.unlink()
        os.link(self.path, link)
        with self.assertRaisesRegex(backup.BackupError, "private_regular_file_required"):
            backup.verify(self.path)
        link.unlink()
        backup.manifest_path(self.path).write_bytes(b"invalid json")
        with self.assertRaisesRegex(backup.BackupError, "invalid_manifest"):
            backup.verify(self.path)

    def test_existing_restore_target_is_not_restored_or_dropped(self):
        self.make_archive()
        self.backend.existing.add("new_database")
        with self.assertRaises(backup.BackupError):
            self.restore()
        self.assertEqual(self.backend.created, [])
        self.assertEqual(self.backend.restored, [])
        self.assertEqual(self.backend.dropped, [])

    def test_native_archive_rejection_never_creates_a_database(self):
        self.make_archive()
        self.backend.fail_archive_list = True
        with self.assertRaises(backup.BackupError):
            self.restore()
        self.assertEqual(self.backend.created, [])
        self.assertEqual(self.backend.dropped, [])

    def test_restore_drill_cleans_only_its_created_database(self):
        self.make_archive()
        result = self.restore()
        self.assertTrue(result["verified"])
        self.assertEqual(self.backend.created, ["new_database"])
        self.assertEqual(self.backend.restored, ["new_database"])
        self.assertEqual(self.backend.dropped, ["new_database"])

    def test_verified_restore_new_is_retained(self):
        self.make_archive()
        self.assertTrue(self.restore(keep=True)["retained"])
        self.assertEqual(self.backend.dropped, [])

    def test_restore_failure_and_evidence_mismatch_clean_created_target(self):
        self.make_archive()
        self.backend.fail_restore = True
        with self.assertRaises(backup.BackupError):
            self.restore(keep=True)
        self.assertEqual(self.backend.dropped, ["new_database"])
        self.backend.fail_restore = False
        self.backend.dropped.clear()
        with self.assertRaisesRegex(backup.BackupError, "restored_evidence_mismatch"):
            self.restore(keep=True, evidence={**EVIDENCE, "critical_sums": {"public.orders.price_minor": "1"}})
        self.assertEqual(self.backend.dropped, ["new_database"])

    def test_public_parent_and_symlink_parent_rejected(self):
        self.root.chmod(0o755)
        with self.assertRaisesRegex(backup.BackupError, "private_parent_directory_required"):
            self.make_archive()
        self.root.chmod(0o700)
        alias = self.root / "alias"
        alias.symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(backup.BackupError, "symlink_path_rejected"):
            backup.private_parent(alias)

    def test_database_arguments_are_names_and_not_connection_uris_or_shell(self):
        for name in ("postgres://secret@example.test/db", "db;DROP DATABASE other", "../database", "", "A"):
            with self.assertRaises(backup.BackupError):
                backup.database_name(name)
        backend = backup.Backend(Path("compose.yaml"), None, "db", "xui", 60)
        command = backend.command("pg_dump", "-d", "fixture")
        self.assertNotIn("--password", command)
        self.assertNotIn("--clean", command)
        self.assertIn('export PGPASSWORD="${POSTGRES_PASSWORD:-}"; exec timeout "$@"', command)

    def test_fixture_catalog_tracks_the_real_migration_list(self):
        sql = backup.fixture_migrations()
        self.assertIn("CREATE TABLE fund_ledger_accounts", sql)
        self.assertIn("INSERT INTO schema_migrations(version) VALUES(15)", sql)
        self.assertTrue(sql.endswith("COMMIT;\n"))


if __name__ == "__main__":
    unittest.main()
