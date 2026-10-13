#!/usr/bin/env python3
"""Generate fixed-input solid RAR3/RAR5 volumes using external RAR 6.24.

No archiver binary is bundled or downloaded. Supply the official tool with
--rar, and choose an empty output directory with --out. Clear archives are
bit-identical; --encrypted uses random salt/IV but preserves the same plaintext.
"""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile

RAR_BINARY_SHA256 = "71ab9088d575e438d243672fcc6d36d2f9a39f5a8d4d26e998129cea12dbf682"


def payload():
    state = 0x86
    block = bytearray()
    for _ in range(49152):
        state ^= (state << 13) & 0xFFFFFFFF
        state ^= state >> 17
        state ^= (state << 5) & 0xFFFFFFFF
        state &= 0xFFFFFFFF
        block.append(state & 0xFF)
    return bytes(block) * 2


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rar", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--encrypted", action="store_true", help="use only the synthetic fixture password: password")
    args = parser.parse_args()
    rar = args.rar.resolve()
    if hashlib.sha256(rar.read_bytes()).hexdigest() != RAR_BINARY_SHA256:
        parser.error("expected the official RAR 6.24 Linux x64 binary recorded in SOURCE.txt")
    output = args.out.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.glob("*.rar")):
        parser.error("output directory already contains RAR files")
    env = dict(os.environ, TZ="UTC", LC_ALL="C")
    with tempfile.TemporaryDirectory(prefix="solid-multipart-") as directory:
        for name in ("first.bin", "second.bin"):
            path = Path(directory) / name
            path.write_bytes(payload())
            path.chmod(0o644)
            os.utime(path, (946684800, 946684800))
        for version in (3, 5):
            suffix = "solid-encrypted-multipart" if args.encrypted else "solid-multipart"
            command = [
                str(rar), "a", "-cfg-", "-ma4" if version == 3 else "-ma5",
                "-m3", "-md256k", "-s", "-sv-", "-v16384b", "-mt1",
                "-ep", "-ts-", "-oi0", "-idq",
                *(["-ppassword"] if args.encrypted else []),
                str(output / f"rar{version}-{suffix}.rar"),
                "first.bin", "second.bin",
            ]
            subprocess.run(command, cwd=directory, env=env, check=True)
    for path in sorted(output.glob("*.rar")):
        data = path.read_bytes()
        print(path.name, len(data), hashlib.sha256(data).hexdigest())


if __name__ == "__main__":
    main()
