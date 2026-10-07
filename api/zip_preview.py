"""Bounded, read-only ZIP access over HTTP byte ranges; never extracts paths to disk."""

import io
import hashlib
import re
import struct
import time
import zipfile
import tempfile
from contextlib import contextmanager
from dataclasses import dataclass

import requests
import pyzipper

INDEX_LIMIT = 8 * 1024 * 1024
FILE_LIMIT = 32 * 1024 * 1024
ENTRY_LIMIT = 20000


class ZipPreviewError(ValueError):
    pass


class ArchivePasswordRequired(ZipPreviewError):
    pass


class ExpiredArchiveLink(ZipPreviewError):
    pass


class ChangedArchive(ZipPreviewError):
    pass


@dataclass(frozen=True)
class RangeSnapshot:
    size: int
    validator: str | None
    blocks: tuple

    @property
    def version(self):
        digest = hashlib.sha256(str(self.size).encode())
        for offset, data in self.blocks:
            digest.update(str(offset).encode() + b":" + data)
        return digest.hexdigest()[:24]

    @property
    def byte_size(self):
        return sum(len(data) for _, data in self.blocks)


class RangeReader(io.RawIOBase):
    def __init__(self, url, snapshot=None, record=False):
        self.url = url
        self.session = requests.Session()
        self.position = 0
        self.buffer_start = 0
        self.buffer = b""
        self.read_ahead = 0
        self.size = snapshot.size if snapshot else None
        self.validator = snapshot.validator if snapshot else None
        self.index_blocks = snapshot.blocks if snapshot else ()
        self.record = record
        self.recorded_blocks = []
        self.remaining = INDEX_LIMIT * 2
        self.deadline = time.monotonic() + 60
        try:
            if snapshot is None:
                self._fetch(0, 0)
        except Exception:
            self.close()
            raise

    def close(self):
        self.session.close()
        super().close()

    def readable(self):
        return True

    def seekable(self):
        return True

    def tell(self):
        return self.position

    def seek(self, offset, whence=0):
        position = offset + (self.position if whence == 1 else self.size if whence == 2 else 0)
        if whence not in (0, 1, 2) or position < 0:
            raise OSError("ZIP 偏移无效")
        self.position = position
        return position

    def snapshot(self):
        return RangeSnapshot(self.size, self.validator, tuple(self.recorded_blocks))

    def _fetch(self, start, end):
        for offset, data in self.index_blocks:
            if offset <= start and end < offset + len(data):
                return data[start - offset : end - offset + 1]
        length = end - start + 1
        if length > INDEX_LIMIT:
            raise ZipPreviewError("ZIP 单次读取过大（索引上限 8 MiB）")
        if length > self.remaining or time.monotonic() > self.deadline:
            raise ZipPreviewError("ZIP 读取超过大小或时间限制")
        headers = {"Range": f"bytes={start}-{end}", "Accept-Encoding": "identity"}
        if self.validator:
            headers["If-Range"] = self.validator
        with self.session.get(self.url, headers=headers, stream=True, timeout=(5, 15)) as response:
            if response.status_code in (401, 403, 404, 410):
                raise ExpiredArchiveLink("压缩包下载链接已失效")
            if response.status_code == 200 and self.validator:
                raise ChangedArchive("压缩包已变化或下载服务不再支持分段读取，请刷新目录")
            if response.status_code != 206:
                raise ZipPreviewError("下载地址不支持分段读取或链接已失效，请重新打开或下载 ZIP 后查看")
            match = re.fullmatch(r"bytes (\d+)-(\d+)/(\d+)", response.headers.get("Content-Range", ""))
            if not match or tuple(map(int, match.groups()[:2])) != (start, end):
                raise ZipPreviewError("下载服务返回了错误的数据范围")
            size = int(match[3])
            if size <= end or (self.size is not None and self.size != size):
                raise ChangedArchive("压缩包已变化，请刷新目录后重新打开")
            validator = response.headers.get("ETag")
            if not validator or validator.startswith("W/"):
                validator = response.headers.get("Last-Modified")
            if self.validator and validator != self.validator:
                raise ChangedArchive("压缩包已变化，请刷新目录后重新打开")
            if response.headers.get("Content-Encoding", "identity") != "identity":
                raise ZipPreviewError("下载服务不支持原始字节读取")
            self.size, self.validator = size, validator
            data = bytearray()
            for chunk in response.iter_content(64 * 1024):
                if getattr(self, "cancel_event", None) is not None and self.cancel_event.is_set():
                    raise InterruptedError("已取消")
                data.extend(chunk)
                if len(data) > length or time.monotonic() > self.deadline:
                    raise ZipPreviewError("ZIP 读取超过大小或时间限制")
            if len(data) != length:
                raise ZipPreviewError("ZIP 数据不完整")
            self.remaining -= length
            result = bytes(data)
            if self.record:
                self.recorded_blocks.append((start, result))
            return result

    def prefetch(self, start, size):
        """Fetch a bounded member in larger ranges, then decompress locally."""
        if size > FILE_LIMIT or start + size > self.size:
            raise ZipPreviewError("ZIP 文件数据范围无效")
        chunks = []
        for offset in range(0, size, INDEX_LIMIT):
            chunks.append(self._fetch(start + offset, start + min(offset + INDEX_LIMIT, size) - 1))
        self.buffer_start = start
        self.buffer = b"".join(chunks)

    def read(self, size=-1):
        size = self.size - self.position if size is None or size < 0 else min(size, self.size - self.position)
        if size <= 0:
            return b""
        relative = self.position - self.buffer_start
        if relative >= 0 and relative + size <= len(self.buffer):
            data = self.buffer[relative : relative + size]
        else:
            if self.read_ahead and size <= self.read_ahead:
                self.buffer_start = self.position
                self.buffer = self._fetch(self.position, min(self.size, self.position + self.read_ahead) - 1)
                data = self.buffer[:size]
            else:
                data = self._fetch(self.position, self.position + size - 1)
        self.position += len(data)
        return data


@contextmanager
def open_remote_zip(url, snapshot=None, record=False, password=None):
    with RangeReader(url, snapshot=snapshot, record=record) as source:
        # Reject split archives before zipfile interprets their offsets.
        source.seek(max(0, source.size - 65557))
        tail = source.read()
        end = tail.rfind(b"PK\x05\x06")
        if end < 0 or len(tail) - end < 22:
            raise ZipPreviewError("不是有效的 ZIP 文件")
        fields = struct.unpack_from("<4s4H2LH", tail, end)
        if fields[1] or fields[2] or fields[3] != fields[4]:
            raise ZipPreviewError("暂不支持分卷 ZIP")
        locator = end - 20
        if locator >= 0 and tail[locator : locator + 4] == b"PK\x06\x07":
            _, disk, _, disks = struct.unpack_from("<4sLQL", tail, locator)
            if disk or disks != 1:
                raise ZipPreviewError("暂不支持分卷 ZIP")
        if fields[5] != 0xFFFFFFFF and fields[5] > INDEX_LIMIT:
            raise ZipPreviewError("ZIP 目录索引过大（上限 8 MiB）")
        try:
            with pyzipper.AESZipFile(source) as archive:
                if password is not None:
                    archive.setpassword(password.encode("utf-8"))
                if len(archive.infolist()) > ENTRY_LIMIT:
                    raise ZipPreviewError("ZIP 文件条目过多（上限 20000）")
                yield archive, source
        except pyzipper.zipfile.BadZipFile as exc:
            raise zipfile.BadZipFile(str(exc)) from exc


def read_member(archive, source, entry, max_size=FILE_LIMIT, output_file=False, output_path=None, on_progress=None):
    try:
        return _read_member(archive, source, entry, max_size, output_file, output_path, on_progress)
    except pyzipper.zipfile.BadZipFile as exc:
        raise zipfile.BadZipFile(str(exc)) from exc


def _read_member(archive, source, entry, max_size=FILE_LIMIT, output_file=False, output_path=None, on_progress=None):
    if entry.is_dir():
        raise ZipPreviewError("请选择文件")
    if entry.flag_bits & 1 and not archive.pwd:
        raise ArchivePasswordRequired("文件已加密，请输入解压密码")
    if entry.compress_type not in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED):
        raise ZipPreviewError("暂不支持此 ZIP 压缩方式，仅支持 Store / Deflate")
    if max_size is not None and max(entry.file_size, entry.compress_size) > max_size:
        raise ZipPreviewError("单文件预览和下载上限为 32 MiB，请下载原 ZIP")
    output_limit = entry.file_size if max_size is None else max_size
    source.remaining += (entry.compress_size + 2 * INDEX_LIMIT if max_size is None else FILE_LIMIT) + 65536
    # Large images read compressed data in bounded windows, not a second full copy.
    if max_size is None:
        source.read_ahead = min(INDEX_LIMIT, max(65536, entry.compress_size + 65536))
    # Unbounded image previews go to disk.  ZipExtFile validates CRC only after
    # EOF, so the complete member is checked before this file is returned.
    output = (open(output_path, "xb") if output_path is not None else tempfile.TemporaryFile(mode="w+b")) if output_file and max_size is None else None
    chunks, total = [], 0
    if entry.flag_bits & 1 and max_size is not None:
        # Include the encryption header and authentication trailer in the range.
        source.seek(entry.header_offset)
        header = source.read(30)
        if len(header) != 30 or header[:4] != b"PK\x03\x04":
            raise ZipPreviewError("ZIP 文件头损坏")
        name_size, extra_size = struct.unpack_from("<HH", header, 26)
        source.prefetch(entry.header_offset + 30 + name_size + extra_size, entry.compress_size)
    try:
        with archive.open(entry) as member:
            if not entry.flag_bits & 1 and max_size is not None:
                source.prefetch(source.tell(), entry.compress_size)
            while True:
                if getattr(source, "cancel_event", None) is not None and source.cancel_event.is_set():
                    raise InterruptedError("已取消")
                chunk = member.read(64 * 1024)
                if not chunk:
                    break
                total += len(chunk)
                if total > output_limit or time.monotonic() > source.deadline:
                    raise ZipPreviewError("文件解压超过大小或时间限制")
                if output is None:
                    chunks.append(chunk)
                else:
                    output.write(chunk)
                if on_progress:
                    on_progress(total, entry.file_size)
        if output is not None:
            output.flush()
            if output_path is not None:
                output.close()
                return output_path
            output.seek(0)
            return output
        return b"".join(chunks)
    except BaseException:
        if output is not None:
            output.close()
        raise
