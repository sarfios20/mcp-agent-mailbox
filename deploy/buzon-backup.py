#!/usr/bin/env python3
"""Create verified SQLite backups and retain sparse historical snapshots."""

from __future__ import annotations

import argparse
import hashlib
import os
import re
import sqlite3
from collections.abc import Callable, Iterable
from contextlib import closing
from datetime import UTC, datetime
from pathlib import Path

SNAPSHOT_RE = re.compile(r"buzon-(\d{8}T\d{6}Z)\.sqlite")
try:
    import fcntl
except ImportError:  # Lets retention tests run on Windows; deployment is Linux-only.
    fcntl = None


def snapshot_time(path: Path) -> datetime | None:
    match = SNAPSHOT_RE.fullmatch(path.name)
    if not match:
        return None
    try:
        return datetime.strptime(match.group(1), "%Y%m%dT%H%M%SZ").replace(tzinfo=UTC)
    except ValueError:
        return None


def checksum_path(snapshot: Path) -> Path:
    return snapshot.with_suffix(".sha256")


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def checksum_matches(snapshot: Path) -> bool:
    sidecar = checksum_path(snapshot)
    if not sidecar.is_file() or sidecar.is_symlink():
        return False
    fields = sidecar.read_text(encoding="ascii").strip().split()
    return len(fields) == 2 and fields[1] == snapshot.name and fields[0] == digest(snapshot)


def bucket_functions() -> tuple[tuple[str, int, Callable[[datetime], object]], ...]:
    return (
        ("daily", 3, lambda stamp: stamp.date()),
        ("weekly", 2, lambda stamp: stamp.isocalendar()[:2]),
        ("monthly", 12, lambda stamp: (stamp.year, stamp.month)),
        ("yearly", 1, lambda stamp: stamp.year),
    )


def select_retained(snapshots: Iterable[Path]) -> set[Path]:
    dated = [(path, snapshot_time(path)) for path in snapshots]
    ordered = sorted(
        ((path, stamp) for path, stamp in dated if stamp is not None),
        key=lambda item: item[1],
        reverse=True,
    )
    retained: set[Path] = set()
    for _name, limit, bucket_for in bucket_functions():
        buckets: set[object] = set()
        for path, stamp in ordered:
            bucket = bucket_for(stamp)
            if bucket in buckets:
                continue
            if len(buckets) >= limit:
                break
            buckets.add(bucket)
            retained.add(path)
    if ordered:
        retained.add(ordered[0][0])
    return retained


def verified_snapshots(root: Path) -> list[Path]:
    snapshots: list[Path] = []
    for path in root.iterdir():
        if path.is_file() and not path.is_symlink() and snapshot_time(path) is not None:
            try:
                verified = checksum_matches(path)
                if verified:
                    verify_database(path)
            except (OSError, RuntimeError, sqlite3.Error):
                verified = False
            if verified:
                snapshots.append(path)
            else:
                print(f"warning: preserving unverified snapshot {path}")
    return snapshots


def prune(root: Path) -> list[Path]:
    snapshots = verified_snapshots(root)
    retained = select_retained(snapshots)
    removed: list[Path] = []
    for snapshot in snapshots:
        if snapshot in retained:
            continue
        sidecar = checksum_path(snapshot)
        snapshot.unlink()
        sidecar.unlink()
        removed.append(snapshot)
    return removed


def verify_database(path: Path) -> None:
    with closing(sqlite3.connect(f"file:{path}?mode=ro&immutable=1", uri=True)) as connection:
        result = connection.execute("PRAGMA integrity_check").fetchone()
    if result is None or result[0] != "ok":
        raise RuntimeError(f"SQLite integrity check failed for {path}: {result}")


def create_backup(database: Path, root: Path, now: datetime | None = None) -> Path:
    if not database.is_file():
        raise FileNotFoundError(f"mailbox database does not exist: {database}")
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    stamp = (now or datetime.now(UTC)).astimezone(UTC).strftime("%Y%m%dT%H%M%SZ")
    destination = root / f"buzon-{stamp}.sqlite"
    temporary = root / f".{destination.name}.tmp"
    temporary_checksum = root / f".{destination.name}.sha256.tmp"
    if destination.exists():
        raise FileExistsError(f"backup already exists: {destination}")

    try:
        with closing(sqlite3.connect(f"file:{database}?mode=ro", uri=True)) as source:
            with closing(sqlite3.connect(temporary)) as target:
                source.backup(target)
                target.commit()
                journal_mode = target.execute("PRAGMA journal_mode=DELETE").fetchone()
                if journal_mode is None or journal_mode[0].lower() != "delete":
                    raise RuntimeError(f"could not finalize backup journal: {journal_mode}")
        verify_database(temporary)
        checksum = digest(temporary)
        temporary_checksum.write_text(f"{checksum}  {destination.name}\n", encoding="ascii")
        os.chmod(temporary, 0o600)
        os.chmod(temporary_checksum, 0o600)
        temporary.replace(destination)
        temporary_checksum.replace(checksum_path(destination))
    except Exception:
        temporary.unlink(missing_ok=True)
        temporary_checksum.unlink(missing_ok=True)
        Path(f"{temporary}-wal").unlink(missing_ok=True)
        Path(f"{temporary}-shm").unlink(missing_ok=True)
        raise

    prune(root)
    return destination


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--database", type=Path, default=Path("/var/lib/buzon/buzon.db"))
    parser.add_argument("--backup-dir", type=Path, default=Path("/var/backups/buzon"))
    parser.add_argument("--lock", type=Path, default=Path("/run/lock/buzon-backup.lock"))
    args = parser.parse_args()

    if fcntl is None:
        raise SystemExit("backup locking requires Linux")
    args.lock.parent.mkdir(parents=True, exist_ok=True)
    with args.lock.open("w", encoding="ascii") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise SystemExit("another mailbox backup is already running") from error
        destination = create_backup(args.database, args.backup_dir)
    print(destination)


if __name__ == "__main__":
    main()
