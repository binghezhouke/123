"""Locate unencrypted ZIP video bytes for direct playback or client-side inflation."""

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
class MemberSlice:
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
    return MemberSlice(source.url, source.size, source.validator, offset, entry.file_size)


CLIENT_VIDEO_LIMIT = 64 * 1024 * 1024
CLIENT_VIDEO_TYPES = {".mp4", ".m4v"}


def can_inflate_in_browser(entry, kind, suffix):
    return (
        kind == ".zip"
        and suffix in CLIENT_VIDEO_TYPES
        and not entry.is_dir()
        and not entry.flag_bits & (1 | 0x20 | 0x40 | 0x2000)
        and entry.compress_type == zipfile.ZIP_DEFLATED
        and 0 < entry.compress_size <= CLIENT_VIDEO_LIMIT
        and 0 < entry.file_size <= CLIENT_VIDEO_LIMIT
    )


def locate_deflate_member(archive, source, entry, kind, suffix):
    if not can_inflate_in_browser(entry, kind, suffix):
        raise ZipPreviewError("客户端解压仅支持未加密的 Deflate MP4/M4V，压缩和解压大小均不能超过 64 MiB")
    # open() validates the local filename/flags but does not inflate any payload.
    with archive.open(entry):
        offset = source.tell()
    if offset < 0 or offset + entry.compress_size > archive.start_dir:
        raise ZipPreviewError("ZIP 视频数据范围超出压缩包内容区")
    # Exclude any trailing data descriptor: this is exactly raw Deflate payload.
    return MemberSlice(source.url, source.size, source.validator, offset, entry.compress_size)
