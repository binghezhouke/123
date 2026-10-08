"""Validate archive passwords before publishing cloud password sidecars."""

import time
import zipfile

from .zip_preview import INDEX_LIMIT, ZipPreviewError


def validate_zip_password(archive, source):
    """Read one encrypted member to EOF so its CRC/AES authentication is checked.

    Prefer the smallest nonempty member: an empty ZipCrypto member gives a
    much weaker password check. Mixed-password archives are checked again by
    the normal reader when another member is opened.
    """
    entries = [entry for entry in archive.infolist()
               if not entry.is_dir() and entry.flag_bits & 1
               and entry.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)]
    if not entries:
        raise ZipPreviewError("没有可验证密码的加密 ZIP 文件，未生成密码文件")
    entry = min(entries, key=lambda item: (item.file_size == 0, item.file_size))
    source.remaining += entry.compress_size + 2 * INDEX_LIMIT + 65536
    source.read_ahead = 256 * 1024
    total = 0
    with archive.open(entry) as stream:
        while True:
            if time.monotonic() > source.deadline:
                raise ZipPreviewError("密码验证超时，未生成密码文件，请稍后重试")
            chunk = stream.read(min(1024 * 1024, entry.file_size - total + 1))
            if not chunk:
                break
            total += len(chunk)
            if total > entry.file_size:
                raise ZipPreviewError("加密文件大小不一致，未生成密码文件")
    if total != entry.file_size:
        raise ZipPreviewError("加密文件不完整，未生成密码文件")


def validate_archive_password(archive, source):
    """Validate a supported archive before publishing its password sidecar."""
    from .archive_preview import ArchiveAdapter

    if not isinstance(archive, ArchiveAdapter):
        return validate_zip_password(archive, source)
    entries = [entry for entry in archive.infolist()
               if not entry.is_dir() and entry.flag_bits & 1]
    if not entries:
        raise ZipPreviewError("没有可验证密码的加密文件，未生成密码文件")
    entry = min(entries, key=lambda item: (item.file_size == 0, item.file_size))
    # Use the existing bounded disk extraction and backend CRC checks. Solid
    # streams may need preceding members; ordinary preview limits stay intact.
    with archive.read(entry, max_size=None, output_file=True, validate_password=True):
        pass
