"""Safe local preparation for nested ZIP files."""

import re
import shutil
import stat
import time
import zipfile
from pathlib import Path, PurePosixPath

import pyzipper
import py7zr
import rarfile
from py7zr.io import Py7zIO, WriterFactory

from .archive_jobs import ArchiveJobError
from .archive_names import member_name

MAX_NESTED_BYTES = 24 * 1024**3
MAX_NESTED_ARCHIVE_BYTES = 64 * 1024**3
MAX_NESTED_ENTRIES = 20000


def _safe_name(raw):
    name = raw.replace("\\", "/")
    path = PurePosixPath(name)
    if path.is_absolute() or any(part in ("", ".", "..") for part in path.parts) or re.match(r"^[A-Za-z]:", name):
        raise ArchiveJobError("内层 ZIP 包含不安全的路径")
    return path


def extract_nested_zip(archive_path, output_dir, password, record, jobs):
    """Fully validate and extract a nested ZIP within explicit disk limits."""
    try:
        archive = pyzipper.AESZipFile(archive_path)
    except (OSError, zipfile.BadZipFile) as exc:
        raise ArchiveJobError("内层文件不是有效的 ZIP 压缩包") from exc
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    total = 0
    seen = set()
    try:
        entries = archive.infolist()
        if len(entries) > MAX_NESTED_ENTRIES:
            raise ArchiveJobError("内层 ZIP 文件条目过多（上限 20000）")
        uncompressed_total = sum(info.file_size for info in entries)
        if uncompressed_total > MAX_NESTED_BYTES:
            raise ArchiveJobError("内层 ZIP 解压数据超过 24 GiB 上限")
        if shutil.disk_usage(output).free < uncompressed_total:
            raise ArchiveJobError("磁盘空间不足，无法准备内层 ZIP")
        jobs.reserve_staging(record, uncompressed_total)
        archive.setpassword(password.encode("utf-8") if password is not None else None)
        for index, info in enumerate(entries):
            if record["cancel"].is_set():
                raise InterruptedError("已取消")
            if info.flag_bits & 1 and password is None:
                raise ArchiveJobError("内层 ZIP 需要密码")
            rel = _safe_name(member_name(info))
            key = str(rel)
            if key in seen:
                raise ArchiveJobError("内层 ZIP 含有重复路径，无法安全浏览")
            seen.add(key)
            mode = (info.external_attr >> 16) & 0xFFFF
            if stat.S_ISLNK(mode):
                raise ArchiveJobError("内层 ZIP 含有符号链接，无法安全浏览")
            total += info.file_size
            if total > MAX_NESTED_BYTES:
                raise ArchiveJobError("内层 ZIP 解压数据超过 24 GiB 上限")
            target = output.joinpath(*rel.parts)
            if info.is_dir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            written = 0
            try:
                with archive.open(info, "r", pwd=password.encode("utf-8") if password is not None else None) as src, open(target, "xb") as dst:
                    while True:
                        if record["cancel"].is_set():
                            raise InterruptedError("已取消")
                        if time.monotonic() > record.get("deadline", float("inf")):
                            raise ArchiveJobError("内层 ZIP 准备超过时间上限")
                        chunk = src.read(64 * 1024)
                        if not chunk:
                            break
                        written += len(chunk)
                        if written > info.file_size or written > MAX_NESTED_BYTES:
                            raise ArchiveJobError("内层 ZIP 解压数据异常")
                        dst.write(chunk)
                        jobs.set_phase(record, "正在校验并解压", processed=total - info.file_size + written, total=uncompressed_total)
            except BaseException:
                try:
                    target.unlink()
                except FileNotFoundError:
                    pass
                raise
            if written != info.file_size:
                raise ArchiveJobError("内层 ZIP 文件数据不完整")
        return output
    except (pyzipper.zipfile.BadZipFile, zipfile.BadZipFile, RuntimeError, EOFError) as exc:
        if password is not None:
            raise ArchiveJobError("内层 ZIP 密码错误，或加密数据已损坏") from exc
        raise ArchiveJobError("内层 ZIP 数据损坏") from exc
    finally:
        archive.close()


class _DiskWriter(Py7zIO):
    def __init__(self, path, limit, expected_size, record, jobs, total, processed_base):
        self.path, self.limit, self.record, self.jobs, self.total = path, limit, record, jobs, total
        self.expected_size = expected_size
        self.processed_base = processed_base
        self.closed_size = None
        self.stream = open(path, "xb")

    def write(self, data):
        if self.record["cancel"].is_set():
            raise InterruptedError("已取消")
        if time.monotonic() > self.record.get("deadline", float("inf")):
            raise ArchiveJobError("内层 7z 准备超过时间上限")
        if self.stream.tell() + len(data) > self.limit:
            raise ArchiveJobError("内层 7z 解压数据超过磁盘预算")
        count = self.stream.write(data)
        self.jobs.set_phase(self.record, "正在校验并解压", self.processed_base + self.stream.tell(), self.total)
        return count

    def read(self, size=None):
        return self.stream.read(size)

    def seek(self, offset, whence=0):
        return self.stream.seek(offset, whence)

    def flush(self):
        return self.stream.flush()

    def size(self):
        if self.closed_size is not None:
            return self.closed_size
        position = self.stream.tell()
        self.stream.seek(0, 2)
        size = self.stream.tell()
        self.stream.seek(position)
        return size

    def close(self):
        if self.closed_size is not None:
            return
        actual = self.size()
        self.stream.close()
        self.closed_size = actual
        if actual != self.expected_size:
            raise ArchiveJobError("内层 7z 成员大小与目录记录不符")


class _DiskFactory(WriterFactory):
    def __init__(self, root, names, info_by_name, record, jobs, total):
        self.root, self.names, self.info_by_name = root, names, info_by_name
        self.record, self.jobs, self.total = record, jobs, total
        self.writers = []
        self.processed_base = 0

    def create(self, filename):
        rel = self.names.get(filename)
        if rel is None:
            raise ArchiveJobError("内层 7z 包含无法识别的文件路径")
        info = self.info_by_name[filename]
        writer = _DiskWriter(self.root.joinpath(*rel.parts), info.uncompressed or 0,
                             info.uncompressed or 0, self.record, self.jobs, self.total, self.processed_base)
        self.writers.append(writer)
        self.processed_base += info.uncompressed or 0
        return writer


def extract_nested_7z(archive_path, output_dir, password, record, jobs):
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    try:
        archive = py7zr.SevenZipFile(archive_path, mode="r", password=password, max_extract_size=MAX_NESTED_BYTES)
    except (OSError, py7zr.exceptions.ArchiveError) as exc:
        raise ArchiveJobError("内层文件不是有效的 7z 压缩包，或密码不正确") from exc
    try:
        factory = None
        infos = archive.list()
        if len(infos) > MAX_NESTED_ENTRIES:
            raise ArchiveJobError("内层 7z 文件条目过多（上限 20000）")
        names, seen, total = {}, set(), 0
        info_by_name = {}
        for info in infos:
            rel = _safe_name(info.filename)
            if str(rel) in seen:
                raise ArchiveJobError("内层 7z 含有重复路径，无法安全浏览")
            seen.add(str(rel))
            names[info.filename] = rel
            info_by_name[info.filename] = info
            total += info.uncompressed or 0
            if total > MAX_NESTED_BYTES:
                raise ArchiveJobError("内层 7z 解压数据超过 24 GiB 上限")
            mode = getattr(info, "posix_mode", 0) or 0
            if stat.S_ISLNK(mode):
                raise ArchiveJobError("内层 7z 含有符号链接，无法安全浏览")
            target = output.joinpath(*rel.parts)
            if info.is_directory:
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
        if shutil.disk_usage(output).free < total:
            raise ArchiveJobError("磁盘空间不足，无法准备内层 7z")
        jobs.reserve_staging(record, total)
        targets = [name for name, info in info_by_name.items() if not info.is_directory]
        factory = _DiskFactory(output, names, info_by_name, record, jobs, total)
        if targets:
            if record["cancel"].is_set():
                raise InterruptedError("已取消")
            archive.extract(targets=targets, factory=factory)
        for name, writer in zip(targets, factory.writers):
            if writer.size() != info_by_name[name].uncompressed:
                raise ArchiveJobError("内层 7z 成员大小与目录记录不符")
        return output
    except (py7zr.exceptions.PasswordRequired, py7zr.exceptions.ArchiveError, RuntimeError, EOFError) as exc:
        raise ArchiveJobError("内层 7z 密码错误，或压缩数据损坏") from exc
    finally:
        if factory is not None:
            for writer in factory.writers:
                try:
                    writer.close()
                except Exception:
                    pass
        archive.close()


def extract_nested_rar(archive_path, output_dir, password, record, jobs):
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    try:
        archive = rarfile.RarFile(archive_path, errors="strict")
        if password is not None:
            archive.setpassword(password)
        if len(archive.volumelist()) > 1:
            raise ArchiveJobError("内层分卷 RAR 暂不支持")
        infos = archive.infolist()
        if len(infos) > MAX_NESTED_ENTRIES:
            raise ArchiveJobError("内层 RAR 文件条目过多（上限 20000）")
        total = sum(info.file_size for info in infos)
        if total > MAX_NESTED_BYTES:
            raise ArchiveJobError("内层 RAR 解压数据超过 24 GiB 上限")
        if shutil.disk_usage(output).free < total:
            raise ArchiveJobError("磁盘空间不足，无法准备内层 RAR")
        jobs.reserve_staging(record, total)
        seen = set()
        processed = 0
        for info in infos:
            if record["cancel"].is_set():
                raise InterruptedError("已取消")
            rel = _safe_name(info.filename)
            if str(rel) in seen:
                raise ArchiveJobError("内层 RAR 含有重复路径，无法安全浏览")
            seen.add(str(rel))
            if info.is_symlink() or info.file_redir or info.flags & (rarfile.RAR_FILE_SPLIT_BEFORE | rarfile.RAR_FILE_SPLIT_AFTER):
                raise ArchiveJobError("内层 RAR 含有链接或分卷文件")
            target = output.joinpath(*rel.parts)
            if info.isdir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            written = 0
            with archive.open(info) as src, open(target, "xb") as dst:
                while True:
                    if record["cancel"].is_set():
                        raise InterruptedError("已取消")
                    if time.monotonic() > record.get("deadline", float("inf")):
                        raise ArchiveJobError("内层 RAR 准备超过时间上限")
                    chunk = src.read(64 * 1024)
                    if not chunk:
                        break
                    written += len(chunk)
                    if written > info.file_size:
                        raise ArchiveJobError("内层 RAR 解压数据异常")
                    dst.write(chunk)
                    jobs.set_phase(record, "正在校验并解压", processed + written, total)
            if written != info.file_size:
                raise ArchiveJobError("内层 RAR 文件数据不完整")
            processed += written
        return output
    except rarfile.PasswordRequired as exc:
        raise ArchiveJobError("内层 RAR 需要密码") from exc
    except rarfile.RarWrongPassword as exc:
        raise ArchiveJobError("内层 RAR 密码错误") from exc
    except rarfile.Error as exc:
        raise ArchiveJobError("内层 RAR 数据损坏、缺少解压工具或算法不支持") from exc
    finally:
        try:
            archive.close()
        except UnboundLocalError:
            pass
