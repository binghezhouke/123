"""Locate unencrypted stored ZIP members for bounded random-access streaming."""

from dataclasses import dataclass
import zipfile

from .zip_preview import ZipPreviewError

VIDEO_TYPES = {".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime"}


def can_stream(entry, kind):
    return (
        kind == ".zip"
        and not entry.is_dir()
        and not entry.flag_bits & (1 | 0x40 | 0x2000)
        and entry.compress_type == zipfile.ZIP_STORED
    )


@dataclass(frozen=True)
class StoredMember:
    url: str
    archive_size: int
    validator: str | None
    offset: int
    size: int


def locate_member(archive, source, entry):
    if not can_stream(entry, ".zip"):
        raise ZipPreviewError("当前仅支持 ZIP 中未加密、仅打包（Store）的视频直接播放")
    if entry.compress_size != entry.file_size:
        raise ZipPreviewError("ZIP 视频大小信息不一致")
    # Use the parser to validate local-header names and flags, including legacy encodings.
    # For an unencrypted stored member open() reads the header but no video payload.
    with archive.open(entry):
        offset = source.tell()
    if offset < 0 or offset + entry.file_size > archive.start_dir:
        raise ZipPreviewError("ZIP 视频数据范围超出压缩包内容区")
    return StoredMember(source.url, source.size, source.validator, offset, entry.file_size)
