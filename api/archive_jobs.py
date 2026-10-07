"""Disk-backed, session-scoped archive preparation jobs and prepared-file cache."""

import os
import shutil
import tempfile
import threading
import time
import uuid
import logging
import json
from queue import Queue
from pathlib import Path
from .zip_preview import ZipPreviewError

logger = logging.getLogger(__name__)


class ArchiveJobError(ValueError):
    pass


class ArchivePreparationJobs:
    """Process-local jobs/cache; owned directories are removed on close and after dead-owner restarts."""

    def __init__(self, root=None, max_bytes=64 * 1024**3, ttl=6 * 3600, workers=1, max_jobs=512):
        parent = Path(root) if root else Path(tempfile.gettempdir())
        parent.mkdir(parents=True, exist_ok=True)
        self._cleanup_dead_owners(parent)
        self.root = Path(tempfile.mkdtemp(prefix="123-archive-cache-", dir=parent))
        self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
        try:
            os.chmod(self.root, 0o700)
        except OSError:
            pass
        (self.root / "owner.json").write_text(
            json.dumps({"pid": os.getpid(), "uid": getattr(os, "getuid", lambda: None)(), "directory": self.root.name}),
            encoding="utf-8",
        )
        try:
            os.chmod(self.root / "owner.json", 0o600)
        except OSError:
            pass
        self.max_bytes, self.ttl, self.max_jobs = max_bytes, ttl, max_jobs
        self.queue = Queue()
        self.worker_count = max(1, workers)
        self.threads = []
        self.lock = threading.RLock()
        self.jobs = {}
        self.cache = {}
        self.staging_reserved = {}
        self.closed = False

    @staticmethod
    def _cleanup_dead_owners(parent):
        for path in parent.glob("123-archive-cache-*"):
            marker = path / "owner.json"
            try:
                owner = json.loads(marker.read_text(encoding="utf-8"))
                if owner.get("directory") != path.name or owner.get("uid") != getattr(os, "getuid", lambda: None)():
                    continue
                owner_pid = int(owner["pid"])
                if owner_pid <= 0:
                    continue
                os.kill(owner_pid, 0)
            except ProcessLookupError:
                shutil.rmtree(path, ignore_errors=True)
            except (FileNotFoundError, PermissionError, ValueError, KeyError, TypeError, OSError):
                # Unknown or inaccessible directories are never removed.
                continue

    def _purge(self):
        now = time.time()
        for key, item in list(self.cache.items()):
            if now - item["created"] > self.ttl:
                shutil.rmtree(item["path"], ignore_errors=True)
                del self.cache[key]
        for job_id, record in list(self.jobs.items()):
            if record["state"] not in ("queued", "running") and now - record["created"] > self.ttl:
                del self.jobs[job_id]
        finished = [record for record in self.jobs.values() if record["state"] not in ("queued", "running")]
        for record in sorted(finished, key=lambda item: item["created"])[:max(0, len(self.jobs) - self.max_jobs)]:
            self.jobs.pop(record["id"], None)

    def reserve_staging(self, record, amount):
        """Account staging bytes against the same disk budget as cached output."""
        amount = max(0, int(amount))
        with self.lock:
            self._purge()
            current = self.staging_reserved.get(record["id"], 0)
            needed = amount
            used = self._usage()
            while used + sum(self.staging_reserved.values()) + needed > self.max_bytes and self.cache:
                oldest_key = min(self.cache, key=lambda k: self.cache[k]["created"])
                old_path = self.cache.pop(oldest_key)["path"]
                shutil.rmtree(old_path, ignore_errors=True)
                used = self._usage()
            if used + sum(self.staging_reserved.values()) + needed > self.max_bytes:
                raise ArchiveJobError("准备暂存和缓存的磁盘预算不足")
            self.staging_reserved[record["id"]] = current + amount

    def release_staging(self, record):
        with self.lock:
            self.staging_reserved.pop(record["id"], None)

    def _usage(self):
        total = 0
        for item in self.cache.values():
            for path in Path(item["path"]).rglob("*"):
                if path.is_file():
                    total += path.stat().st_size
        return total

    def start(self, cache_key, owner, worker):
        with self.lock:
            if self.closed:
                raise ArchiveJobError("后台准备服务已关闭")
            self._purge()
            existing = next((j for j in self.jobs.values() if j["cache_key"] == cache_key and j["owner"] == owner and (j["state"] in ("queued", "running") or (j["state"] == "complete" and cache_key in self.cache))), None)
            if existing:
                return {"id": existing["id"], "cached": existing["state"] == "complete"}
            if len(self.jobs) >= self.max_jobs:
                raise ArchiveJobError("后台准备任务过多，请稍后重试")
            cached = self.cache.get(cache_key)
            if cached:
                cached["created"] = time.time()
                job_id = uuid.uuid4().hex
                self.jobs[job_id] = {"id": job_id, "owner": owner, "cache_key": cache_key, "state": "complete", "phase": "准备缓存命中", "processed": 0, "total": None, "error": None, "cancel": threading.Event(), "path": cached["path"], "created": time.time()}
                return {"id": job_id, "cached": True}
            job_id = uuid.uuid4().hex
            record = {"id": job_id, "owner": owner, "cache_key": cache_key, "state": "queued", "phase": "排队中", "processed": 0, "total": None, "error": None, "cancel": threading.Event(), "path": None, "created": time.time()}
            self.jobs[job_id] = record
            if not self.threads:
                for index in range(self.worker_count):
                    thread = threading.Thread(target=self._consume, name=f"archive-prepare-{index}", daemon=True)
                    self.threads.append(thread)
                    thread.start()
            self.queue.put((record, worker))
            return {"id": job_id, "cached": False}

    def _consume(self):
        # Daemon workers allow the registered shutdown hook to cancel before joining.
        while True:
            item = self.queue.get()
            try:
                if item is None:
                    return
                record, worker = item
                try:
                    self._run(record, worker)
                except Exception as exc:
                    logger.warning("archive worker failed (%s)", type(exc).__name__)
                    with self.lock:
                        record.update(state="failed", phase="准备失败", error="后台准备失败，请稍后重试")
            finally:
                self.queue.task_done()

    def set_phase(self, record, phase, processed=None, total=None):
        with self.lock:
            if record["state"] == "running":
                record["phase"] = phase
                if processed is not None:
                    record["processed"] = processed
                if total is not None:
                    record["total"] = total

    def _run(self, record, worker):
        with self.lock:
            if record["cancel"].is_set():
                record["state"] = "cancelled"
                return
            record["state"] = "running"
        try:
            staging = Path(tempfile.mkdtemp(prefix="job-", dir=self.root))
        except OSError:
            with self.lock:
                record.update(state="failed", phase="准备失败", error="无法创建暂存文件，请检查磁盘空间")
            return
        final_path = self.root / f"cache-{record['id']}"
        try:
            path = worker(staging, record)
            if record["cancel"].is_set():
                raise InterruptedError("已取消")
            with self.lock:
                self.staging_reserved.pop(record["id"], None)
                self._purge()
                used = self._usage()
                size = sum(p.stat().st_size for p in Path(path).rglob("*") if p.is_file())
                if size > self.max_bytes:
                    raise ArchiveJobError("准备结果超过缓存容量上限")
                while used + size > self.max_bytes and self.cache:
                    oldest_key = min(self.cache, key=lambda k: self.cache[k]["created"])
                    old_path = self.cache.pop(oldest_key)["path"]
                    shutil.rmtree(old_path, ignore_errors=True)
                    used = self._usage()
                if used + size > self.max_bytes:
                    raise ArchiveJobError("准备缓存空间不足，请稍后重试")
                os.replace(path, final_path)
                shutil.rmtree(staging, ignore_errors=True)
                self.cache[record["cache_key"]] = {"path": str(final_path), "created": time.time(), "job_id": record["id"]}
                record.update(state="complete", phase="准备完成", path=str(final_path), processed=size, total=size)
        except InterruptedError:
            self.release_staging(record)
            shutil.rmtree(staging, ignore_errors=True)
            shutil.rmtree(final_path, ignore_errors=True)
            with self.lock:
                record.update(state="cancelled", phase="已取消")
        except BaseException as exc:
            self.release_staging(record)
            shutil.rmtree(staging, ignore_errors=True)
            shutil.rmtree(final_path, ignore_errors=True)
            with self.lock:
                message = str(exc) if isinstance(exc, (ArchiveJobError, ZipPreviewError)) else "读取或解压失败，请检查密码、格式和磁盘空间后重试"
                logger.warning("archive preparation failed (%s)", type(exc).__name__)
                record.update(state="failed", phase="准备失败", error=message or "无法准备压缩包")

    def status(self, job_id, owner):
        with self.lock:
            self._purge()
            record = self.jobs.get(job_id)
            if record is None or record["owner"] != owner:
                return None
            if record["state"] == "complete" and record["cache_key"] not in self.cache:
                record.update(state="expired", phase="准备缓存已过期", error="缓存已清理，请重新准备内层压缩包")
            return {k: record[k] for k in ("id", "state", "phase", "processed", "total", "error")}

    def cancel(self, job_id, owner):
        with self.lock:
            self._purge()
            record = self.jobs.get(job_id)
            if record is None or record["owner"] != owner:
                return False
            if record["state"] == "queued":
                record["cancel"].set()
                record.update(state="cancelled", phase="已取消")
            elif record["state"] == "running":
                record["cancel"].set()
                record["phase"] = "正在取消"
            return True

    def prepared_path(self, job_id, owner):
        with self.lock:
            self._purge()
            record = self.jobs.get(job_id)
            if record is None or record["owner"] != owner or record["state"] != "complete":
                return None
            item = self.cache.get(record["cache_key"])
            if item is None:
                record.update(state="expired", phase="准备缓存已过期", error="缓存已清理，请重新准备内层压缩包")
                return None
            item["created"] = time.time()
            return item["path"]

    def close(self):
        with self.lock:
            if self.closed:
                return
            self.closed = True
            for record in self.jobs.values():
                if record["state"] in ("queued", "running"):
                    record["cancel"].set()
        for _ in self.threads:
            self.queue.put(None)
        for thread in self.threads:
            thread.join()
        shutil.rmtree(self.root, ignore_errors=True)
