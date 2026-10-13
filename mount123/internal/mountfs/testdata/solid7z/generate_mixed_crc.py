#!/usr/bin/env python3
"""Emit a synthetic 7z with inherited and explicit substream CRCs (#101)."""

import argparse
from pathlib import Path
import struct
import zlib


def uint(value):
    for count in range(8):
        if value < 1 << (7 * (count + 1)):
            prefix = (0xFF << (8 - count) & 0xFF) | (value >> (8 * count))
            return bytes([prefix]) + value.to_bytes(8, "little")[:count]
    return b"\xff" + value.to_bytes(8, "little")


def crc(data):
    return struct.pack("<I", zlib.crc32(data))


def archive():
    body = b"ABCDEFGHI"
    # Header, main streams, two packed Copy inputs of sizes 1 and 8;
    # only the first folder declares a digest (presence bitmap 0x80).
    header = bytes([1, 4, 6, 0, 2, 9, 1, 8, 0, 7, 11, 2, 0,
                    1, 1, 0, 1, 1, 0, 12, 1, 8, 10, 0, 0x80])
    header += crc(body[:1]) + bytes([0, 8, 13, 1, 8, 9])
    # Eight explicit substream digests. The inherited first file consumes
    # no slot: one presence byte is correct, despite nine files in total.
    header += bytes([1] * 7) + bytes([10, 0, 0xFF])
    header += b"".join(crc(body[i:i + 1]) for i in range(1, 9))
    header += bytes([0, 0, 5, 9, 17])
    names = ["standalone.bin"] + [f"member-{i:02}.bin" for i in range(1, 9)]
    encoded = b"\x00" + b"".join((name + "\0").encode("utf-16le") for name in names)
    header += uint(len(encoded)) + encoded + b"\x00\x00"
    start = struct.pack("<QQI", len(body), len(header), zlib.crc32(header))
    return bytes.fromhex("377abcaf271c0004") + crc(start) + start + body + header


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    with args.output.open("xb") as output:
        output.write(archive())
