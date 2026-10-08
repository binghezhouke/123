"""Read-only non-solid 7z/RAR adapters sharing the bounded HTTP range reader."""

import io
import errno
import os
import time
import lzma
import zlib
import zipfile
import struct
import pyzipper
import tempfile
from threading import Timer
from contextlib import contextmanager, ExitStack
from dataclasses import dataclass

import py7zr
import rarfile
from py7zr.io import Py7zIO, WriterFactory

from .split_archive import VolumeSet, SplitRangeReader
from .zip_preview import (
    ENTRY_LIMIT,
    FILE_LIMIT,
    INDEX_LIMIT,
    RangeReader,
    ZipPreviewError,
    ArchivePasswordRequired,
    open_remote_zip,
    read_member,
)


@dataclass
class Entry:
    filename: str
    file_size: int
    directory: bool
    original: object
    flag_bits: int = 0

    def is_dir(self):
        return self.directory


class BoundedWriter(Py7zIO):
    def __init__(self, deadline, max_size=FILE_LIMIT, cancel_event=None, output_path=None, on_progress=None, total=None):
        self.output_path = output_path
        self.data = (open(output_path, "xb") if output_path is not None else tempfile.TemporaryFile(mode="w+b")) if max_size is None else io.BytesIO()
        self.deadline = deadline
        self.max_size = max_size
        self.cancel_event = cancel_event
        self.on_progress, self.total = on_progress, total

    def write(self, data):
        if self.cancel_event is not None and self.cancel_event.is_set():
            raise InterruptedError("已取消")
        if (self.max_size is not None and self.data.tell() + len(data) > self.max_size) or time.monotonic() > self.deadline:
            raise ZipPreviewError("文件解压超过大小或时间限制")
        result = self.data.write(data)
        if self.on_progress:
            self.on_progress(self.data.tell(), self.total)
        return result

    def read(self, size=None):
        return self.data.read(size)

    def seek(self, offset, whence=0):
        return self.data.seek(offset, whence)

    def flush(self):
        pass

    def size(self):
        position = self.data.tell()
        self.data.seek(0, 2)
        result = self.data.tell()
        self.data.seek(position)
        return result


class Factory(WriterFactory):
    def __init__(self, deadline, max_size=FILE_LIMIT, cancel_event=None, output_path=None, on_progress=None, total=None):
        self.writer = BoundedWriter(deadline, max_size, cancel_event, output_path, on_progress, total)
        self.created = False

    def create(self, filename):
        if self.created:
            raise ZipPreviewError("暂不支持同名或关联文件的提取")
        self.created = True
        return self.writer


class ArchiveAdapter:
    def __init__(self, archive, source, kind, password=None):
        self.password = password
        self.archive, self.source, self.kind = archive, source, kind
        if kind == ".7z":
            # archiveinfo() requires a local filename in py7zr 1.1; inspect stream counts instead.
            streams = archive.header.main_streams
            substreams = streams.substreamsinfo if streams else None
            self.solid = bool(substreams and any(n > 1 for n in substreams.num_unpackstreams_folders))
            self.entries = [
                Entry(
                    f.filename.rstrip("/") + ("/" if f.is_directory else ""),
                    f.uncompressed,
                    f.is_directory,
                    f,
                    int(archive.needs_password()),
                )
                for f in archive.list()
            ]
        else:
            self.solid = archive.is_solid()
            self.entries = [
                Entry(f.filename, f.file_size, f.isdir(), f, int(f.needs_password())) for f in archive.infolist()
            ]
        if len(self.entries) > ENTRY_LIMIT:
            raise ZipPreviewError("压缩包条目过多（上限 20000）")

    def infolist(self):
        return self.entries

    def read(self, entry, max_size=FILE_LIMIT, output_file=False, output_path=None, on_progress=None, validate_password=False):
        if self.solid and not validate_password:
            raise ZipPreviewError("固实压缩包暂仅支持目录浏览，请下载原文件")
        if entry.directory:
            raise ZipPreviewError("请选择文件")
        if entry.flag_bits and self.password is None:
            raise ArchivePasswordRequired("文件已加密，请输入解压密码")
        if max_size is not None and entry.file_size > max_size:
            raise ZipPreviewError("单文件上限为 32 MiB")
        output_limit = entry.file_size if max_size is None else max_size
        compressed = (entry.original.compressed if self.kind == ".7z" else entry.original.compress_size)
        if max_size is None:
            self.source.read_ahead = min(INDEX_LIMIT, max(256 * 1024, compressed or 0))
        self.source.remaining += (
            (compressed if compressed is not None else self.source.size) + 2 * INDEX_LIMIT
            if max_size is None else FILE_LIMIT
        )
        if self.kind == ".7z":
            f = entry.original
            if max_size is not None and f.compressed is not None and f.compressed > max_size:
                raise ZipPreviewError("单文件压缩数据上限为 32 MiB")
            if not f.is_file or f.is_symlink:
                raise ZipPreviewError("暂不支持链接或特殊文件")
            if sum(e.filename == entry.filename for e in self.entries) != 1:
                raise ZipPreviewError("暂不支持 7z 内的重复文件名")
            factory = Factory(self.source.deadline, None if output_file and max_size is None else output_limit,
                              getattr(self.source, "cancel_event", None), output_path, on_progress, output_limit)
            self.archive.max_extract_size = (sum(e.file_size for e in self.entries)
                                             if self.solid else output_limit)
            if self.solid:
                self.source.remaining += self.source.size
            try:
                self.archive.extract(targets=[f.filename], factory=factory)
                if factory.writer.size() != entry.file_size:
                    raise ZipPreviewError("7z 成员数据与目录声明大小不符")
                data = factory.writer.data
                if output_file and max_size is None and output_path is not None:
                    data.flush()
                    data.close()
                    data = output_path
                elif output_file and max_size is None:
                    data.flush()
                    data.seek(0)
                else:
                    data = data.getvalue()
            except BaseException:
                factory.writer.data.close()
                if output_path is not None:
                    try:
                        os.unlink(output_path)
                    except FileNotFoundError:
                        pass
                raise
        else:
            f = entry.original
            if (
                f.is_symlink()
                or f.file_redir
                or f.flags & (rarfile.RAR_FILE_SPLIT_BEFORE | rarfile.RAR_FILE_SPLIT_AFTER)
            ):
                raise ZipPreviewError("暂不支持链接或分卷文件")
            if max_size is not None and f.compress_size > max_size:
                raise ZipPreviewError("单文件压缩数据上限为 32 MiB")
            # rarfile builds a temporary, single-member RAR for compressed entries.
            # Reject archive features which force its whole-archive fallback.
            parser = self.archive._file_parser
            needs_full = not parser._main or parser._main._must_disable_hack() or f._must_disable_hack()
            with ExitStack() as stack:
                archive = self.archive
                if needs_full:
                    if self.password is None or self.source.size > FILE_LIMIT:
                        raise ZipPreviewError("此 RAR 的加密方式需要完整读取；仅支持不超过 32 MiB 的此类压缩包")
                    # The backend cannot construct a standalone encrypted member.
                    # Allow only a bounded encrypted archive, never an unbounded fallback.
                    self.source.seek(0)
                    self.source.remaining += self.source.size
                    parts = []
                    while self.source.tell() < self.source.size:
                        parts.append(self.source.read(min(1024 * 1024, self.source.size - self.source.tell())))
                    archive = stack.enter_context(rarfile.RarFile(io.BytesIO(b"".join(parts)), errors="strict"))
                    archive.setpassword(self.password)
                    f = archive.getinfo(f.filename)
                stream = stack.enter_context(archive.open(f))
                process = getattr(stream, "_proc", None)
                timer = None
                if process is not None:

                    def stop_process():
                        try:
                            process.kill()
                        except ProcessLookupError:
                            pass

                    timer = Timer(max(0.01, self.source.deadline - time.monotonic()), stop_process)
                    timer.daemon = True
                    timer.start()
                try:
                    if output_file and max_size is None:
                        data = open(output_path, "xb") if output_path is not None else tempfile.TemporaryFile(mode="w+b")
                        try:
                            written = 0
                            while True:
                                if getattr(self.source, "cancel_event", None) is not None and self.source.cancel_event.is_set():
                                    raise InterruptedError("已取消")
                                chunk = stream.read(64 * 1024)
                                if not chunk:
                                    break
                                written += len(chunk)
                                if written > entry.file_size:
                                    raise ZipPreviewError("RAR 成员超过目录声明大小")
                                data.write(chunk)
                                if on_progress:
                                    on_progress(data.tell(), output_limit)
                                if time.monotonic() > self.source.deadline:
                                    raise ZipPreviewError("文件解压超过大小或时间限制")
                            if written != entry.file_size:
                                raise ZipPreviewError("RAR 成员数据不完整")
                            data.flush()
                            if output_path is not None:
                                data.close()
                                data = output_path
                            else:
                                data.seek(0)
                        except BaseException:
                            data.close()
                            if output_path is not None:
                                try:
                                    os.unlink(output_path)
                                except FileNotFoundError:
                                    pass
                            raise
                    else:
                        data = stream.read(output_limit + 1)
                finally:
                    if timer:
                        timer.cancel()
        if (not output_file or max_size is not None) and len(data) > output_limit:
            if output_path is not None:
                try:
                    os.unlink(output_path)
                except FileNotFoundError:
                    pass
            raise ZipPreviewError("文件解压超过大小或时间限制")
        if time.monotonic() > self.source.deadline:
            if output_path is not None:
                try:
                    os.unlink(output_path)
                except FileNotFoundError:
                    pass
            raise ZipPreviewError("文件解压超过大小或时间限制")
        return data


@contextmanager
def open_archive(url, kind, snapshot=None, record=False, resolve_part=None, password=None):
    try:
        if kind == ".zip":
            with open_remote_zip(url, snapshot=snapshot, record=record, password=password) as pair:
                yield pair
            return
        reader = (
            SplitRangeReader(url, resolve_part, snapshot=snapshot, record=record)
            if isinstance(url, VolumeSet)
            else RangeReader(url, snapshot=snapshot, record=record)
        )
        with reader as source:
            if isinstance(source, SplitRangeReader):
                source.check_header()
            source.read_ahead = 256 * 1024
            if kind == ".7z":
                with py7zr.SevenZipFile(source, mode="r", max_extract_size=FILE_LIMIT, password=password) as archive:
                    yield ArchiveAdapter(archive, source, kind, password), source
            elif kind == ".rar":
                with rarfile.RarFile(source, errors="strict") as archive:
                    if password is not None:
                        archive.setpassword(password)
                    elif archive.needs_password() and not archive.infolist():
                        raise ArchivePasswordRequired("目录已加密，请输入解压密码")
                    if len(archive.volumelist()) > 1:
                        raise ZipPreviewError("暂不支持分卷 RAR")
                    yield ArchiveAdapter(archive, source, kind, password), source
            else:
                raise ZipPreviewError("不支持此压缩格式")
    except InterruptedError:
        raise
    except (py7zr.exceptions.PasswordRequired, rarfile.PasswordRequired, rarfile.RarWrongPassword) as exc:
        raise ArchivePasswordRequired("压缩包已加密，请输入正确的解压密码") from exc
    except (
        lzma.LZMAError,
        zlib.error,
        pyzipper.zipfile.BadZipFile,
        zipfile.BadZipFile,
        RuntimeError,
        TypeError,
        EOFError,
        struct.error,
        IndexError,
    ) as exc:
        if password is not None or "password" in str(exc).lower():
            raise ArchivePasswordRequired("密码不正确，或加密数据已损坏，请重新输入密码") from exc
        raise ZipPreviewError("压缩数据损坏或算法不支持") from exc
    except (rarfile.Error, py7zr.exceptions.ArchiveError, OSError, ValueError) as exc:
        if isinstance(exc, OSError) and exc.errno in (errno.ENOSPC, getattr(errno, "EDQUOT", -1)):
            raise
        if isinstance(exc, ZipPreviewError):
            raise
        if password is not None and isinstance(exc, (py7zr.exceptions.ArchiveError, rarfile.BadRarFile, ValueError)):
            raise ArchivePasswordRequired("密码不正确，或加密数据已损坏，请重新输入密码") from exc
        raise ZipPreviewError("压缩包无法读取：格式损坏、分卷、算法不支持或缺少 RAR 解压工具") from exc


def read_archive_member(archive, source, entry, max_size=FILE_LIMIT):
    if isinstance(archive, ArchiveAdapter):
        return archive.read(entry, max_size=max_size)
    return read_member(archive, source, entry, max_size=max_size)


def read_archive_member_file(archive, source, entry, output_path=None, on_progress=None):
    """Extract a validated member to a disk file in bounded memory."""
    if isinstance(archive, ArchiveAdapter):
        return archive.read(entry, max_size=None, output_file=True, output_path=output_path, on_progress=on_progress)
    return read_member(archive, source, entry, max_size=None, output_file=True, output_path=output_path, on_progress=on_progress)
