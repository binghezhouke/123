"""Read-only non-solid 7z/RAR adapters sharing the bounded HTTP range reader."""

import io
import time
import lzma
import zlib
import zipfile
import struct
import pyzipper
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
    def __init__(self, deadline):
        self.data = io.BytesIO()
        self.deadline = deadline

    def write(self, data):
        if self.data.tell() + len(data) > FILE_LIMIT or time.monotonic() > self.deadline:
            raise ZipPreviewError("文件解压超过大小或时间限制")
        return self.data.write(data)

    def read(self, size=None):
        return self.data.read(size)

    def seek(self, offset, whence=0):
        return self.data.seek(offset, whence)

    def flush(self):
        pass

    def size(self):
        return len(self.data.getbuffer())


class Factory(WriterFactory):
    def __init__(self, deadline):
        self.writer = BoundedWriter(deadline)
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

    def read(self, entry):
        if self.solid:
            raise ZipPreviewError("固实压缩包暂仅支持目录浏览，请下载原文件")
        if entry.directory:
            raise ZipPreviewError("请选择文件")
        if entry.flag_bits and self.password is None:
            raise ArchivePasswordRequired("文件已加密，请输入解压密码")
        if entry.file_size > FILE_LIMIT:
            raise ZipPreviewError("单文件上限为 32 MiB")
        self.source.remaining += FILE_LIMIT
        if self.kind == ".7z":
            f = entry.original
            if f.compressed is not None and f.compressed > FILE_LIMIT:
                raise ZipPreviewError("单文件压缩数据上限为 32 MiB")
            if not f.is_file or f.is_symlink:
                raise ZipPreviewError("暂不支持链接或特殊文件")
            if sum(e.filename == entry.filename for e in self.entries) != 1:
                raise ZipPreviewError("暂不支持 7z 内的重复文件名")
            factory = Factory(self.source.deadline)
            self.archive.extract(targets=[f.filename], factory=factory)
            data = factory.writer.data.getvalue()
        else:
            f = entry.original
            if (
                f.is_symlink()
                or f.file_redir
                or f.flags & (rarfile.RAR_FILE_SPLIT_BEFORE | rarfile.RAR_FILE_SPLIT_AFTER)
            ):
                raise ZipPreviewError("暂不支持链接或分卷文件")
            if f.compress_size > FILE_LIMIT:
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
                    data = stream.read(FILE_LIMIT + 1)
                finally:
                    if timer:
                        timer.cancel()
        if len(data) > FILE_LIMIT or time.monotonic() > self.source.deadline:
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
        if isinstance(exc, ZipPreviewError):
            raise
        if password is not None and isinstance(exc, (py7zr.exceptions.ArchiveError, rarfile.BadRarFile, ValueError)):
            raise ArchivePasswordRequired("密码不正确，或加密数据已损坏，请重新输入密码") from exc
        raise ZipPreviewError("压缩包无法读取：格式损坏、分卷、算法不支持或缺少 RAR 解压工具") from exc


def read_archive_member(archive, source, entry):
    if isinstance(archive, ArchiveAdapter):
        return archive.read(entry)
    return read_member(archive, source, entry)
