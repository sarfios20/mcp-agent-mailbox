import importlib.util
import sqlite3
import tempfile
import unittest
from contextlib import closing
from datetime import UTC, datetime
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("buzon-backup.py")
SPEC = importlib.util.spec_from_file_location("buzon_backup", MODULE_PATH)
backup = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(backup)


class BackupTests(unittest.TestCase):
    def test_create_backup_is_valid_and_checksummed(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            database = root / "source.sqlite"
            with closing(sqlite3.connect(database)) as connection:
                connection.execute("PRAGMA journal_mode=WAL")
                connection.execute("CREATE TABLE messages (value TEXT)")
                connection.execute("INSERT INTO messages VALUES ('hello')")
                connection.commit()

            destination = backup.create_backup(
                database,
                root / "backups",
                datetime(2026, 9, 28, 5, 15, tzinfo=UTC),
            )

            backup.verify_database(destination)
            self.assertTrue(backup.checksum_matches(destination))
            self.assertFalse(Path(f"{destination}-wal").exists())
            self.assertFalse(Path(f"{destination}-shm").exists())
            self.assertFalse(any((root / "backups").glob(".*.tmp-*")))
            with closing(sqlite3.connect(destination)) as connection:
                self.assertEqual(connection.execute("SELECT value FROM messages").fetchone()[0], "hello")

    def test_sparse_retention_and_malformed_preservation(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            stamps = [
                "20260928T051500Z",
                "20260927T051500Z",
                "20260926T051500Z",
                "20260925T051500Z",
                "20260914T051500Z",
                "20260831T051500Z",
                "20260731T051500Z",
                "20260630T051500Z",
                "20250531T051500Z",
            ]
            snapshots = []
            for stamp in stamps:
                path = root / f"buzon-{stamp}.sqlite"
                with closing(sqlite3.connect(path)) as connection:
                    connection.execute("CREATE TABLE marker (stamp TEXT)")
                    connection.execute("INSERT INTO marker VALUES (?)", (stamp,))
                    connection.commit()
                backup.checksum_path(path).write_text(
                    f"{backup.digest(path)}  {path.name}\n", encoding="ascii"
                )
                snapshots.append(path)
            malformed = root / "buzon-20240101T000000Z.sqlite"
            malformed.write_bytes(b"unverified")
            backup.checksum_path(malformed).write_text(
                f"{backup.digest(malformed)}  {malformed.name}\n", encoding="ascii"
            )
            unrelated = root / "notes.txt"
            unrelated.write_text("keep me", encoding="ascii")

            removed = backup.prune(root)
            retained = backup.select_retained(snapshots)

            self.assertEqual(set(removed), set(snapshots) - retained)
            self.assertTrue(all(path.exists() for path in retained))
            self.assertTrue(malformed.exists())
            self.assertTrue(backup.checksum_path(malformed).exists())
            self.assertTrue(unrelated.exists())

    def test_failed_backup_does_not_prune(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            existing = root / "backups" / "buzon-20260901T051500Z.sqlite"
            existing.parent.mkdir()
            existing.write_bytes(b"existing")
            backup.checksum_path(existing).write_text(
                f"{backup.digest(existing)}  {existing.name}\n", encoding="ascii"
            )

            with self.assertRaises(FileNotFoundError):
                backup.create_backup(root / "missing.sqlite", existing.parent)

            self.assertTrue(existing.exists())
            self.assertTrue(backup.checksum_path(existing).exists())


if __name__ == "__main__":
    unittest.main()
