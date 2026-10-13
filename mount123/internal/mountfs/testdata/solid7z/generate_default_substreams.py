#!/usr/bin/env python3
"""Emit a synthetic 7z with two default single-file groups (#102)."""

import argparse
from pathlib import Path
import struct
import zlib


def archive():
    payloads = [b"A", b"BC"]
    # Header, MainStreamsInfo, two packed Copy inputs, UnpackInfo, group CRCs.
    header = bytes([1, 4, 6, 0, 2, 9, 1, 2, 0, 7, 11, 2, 0,
                    1, 1, 0, 1, 1, 0, 12, 1, 2, 10, 1])
    header += b"".join(struct.pack("<I", zlib.crc32(p)) for p in payloads)
    # End UnpackInfo and MainStreamsInfo without a SubStreamsInfo property;
    # FilesInfo contains two nonempty files named a and b.
    header += bytes([0, 0, 5, 2, 17, 9, 0, 97, 0, 0, 0, 98, 0, 0, 0, 0, 0])
    body = b"".join(payloads)
    start = struct.pack("<QQI", len(body), len(header), zlib.crc32(header))
    return (bytes.fromhex("377abcaf271c0004")
            + struct.pack("<I", zlib.crc32(start)) + start + body + header)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    with args.output.open("xb") as output:
        output.write(archive())
