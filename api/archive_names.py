"""Decode archive display names without changing the parser's extraction names."""

import codecs
import struct
import zlib

import pyzipper


def member_name(entry):
    """Prefer explicit Unicode metadata, then common unmarked Chinese encodings.

    ZIP predates Unicode: when bit 11 is absent the parser defaults to CP437.
    Encoding inference is ambiguous; retain CP437 unless UTF-8 is valid or a
    strict GB18030 decode contains Chinese characters. Never mutate ZipInfo:
    its original name is needed to validate the local header when extracting.
    """
    name = entry.filename
    if not isinstance(entry, pyzipper.zipfile.ZipInfo) or entry.flag_bits & 0x800:
        return name.replace("\\", "/")
    raw = entry.orig_filename.encode("cp437")
    extra = entry.extra
    offset = 0
    while offset + 4 <= len(extra):
        kind, size = struct.unpack_from("<HH", extra, offset)
        offset += 4
        if offset + size > len(extra):
            break
        payload = extra[offset : offset + size]
        offset += size
        # Info-ZIP Unicode Path field: version, original-name CRC, UTF-8 name.
        if kind != 0x7075 or len(payload) < 5 or payload[0] != 1:
            continue
        if struct.unpack_from("<I", payload, 1)[0] != zlib.crc32(raw):
            continue
        try:
            decoded = payload[5:].decode("utf-8")
        except UnicodeDecodeError:
            continue
        if decoded and "\x00" not in decoded and decoded.endswith("/") == name.endswith("/"):
            return decoded.replace("\\", "/")
    try:
        name = raw.decode("utf-8")
    except UnicodeDecodeError:
        try:
            candidate = raw.decode("gb18030")
        except UnicodeDecodeError:
            pass
        else:
            if any("\u3400" <= char <= "\u9fff" or "\U00020000" <= char <= "\U0002ffff" for char in candidate):
                name = candidate
    return name.split("\x00", 1)[0].replace("\\", "/")


def decode_archive_text(data):
    """Decode BOM-marked text before the UTF-8 / GB18030 fallback."""
    for marks, encoding in (
        ((codecs.BOM_UTF32_LE, codecs.BOM_UTF32_BE), "utf-32"),
        ((codecs.BOM_UTF16_LE, codecs.BOM_UTF16_BE), "utf-16"),
        ((codecs.BOM_UTF8,), "utf-8-sig"),
    ):
        if data.startswith(marks):
            return data.decode(encoding, errors="replace")
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError:
        return data.decode("gb18030", errors="replace")
