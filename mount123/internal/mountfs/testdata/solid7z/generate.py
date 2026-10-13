#!/usr/bin/env python3
"""Generate compact synthetic solid LZMA2 input; no tool is bundled/downloaded."""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile


def payload(seed):
    state = seed
    block = bytearray()
    for _ in range(16384):
        state ^= (state << 13) & 0xFFFFFFFF
        state ^= state >> 17
        state ^= (state << 5) & 0xFFFFFFFF
        state &= 0xFFFFFFFF
        block.append(state & 0xFF)
    size = (4 << 20) + 13
    return (bytes(block) * ((size + len(block) - 1) // len(block)))[:size]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sevenzip", default="7z")
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--variant", choices=("plain", "encrypted", "mixed"), default="plain")
    args = parser.parse_args()
    output = args.out.resolve()
    output.mkdir(parents=True, exist_ok=True)
    archive = output / {"plain": "inner.7z", "encrypted": "encrypted.7z", "mixed": "mixed-encryption.7z"}[args.variant]
    if archive.exists():
        parser.error("output archive already exists")
    with tempfile.TemporaryDirectory(prefix="solid7z-fixture-") as directory:
        names = []
        for index, letter in enumerate("abc"):
            name = f"member-{letter}.bin"
            names.append(name)
            data = payload(4100 + index)
            path = Path(directory) / name
            path.write_bytes(data)
            path.chmod(0o644)
            os.utime(path, (946684800, 946684800))
            print(name, len(data), hashlib.sha256(data).hexdigest())
        command = [
            args.sevenzip, "a", "-t7z", "-m0=LZMA2", "-mx=1", "-md=1m",
            "-ms=on", "-mmt=1", "-mhc=off", "-mtm=off", "-mtc=off", "-mta=off",
        ]
        password = [] if args.variant == "plain" else ["-psynthetic-solid-password"]
        groups = [(names, password)]
        if args.variant == "mixed":
            groups = [(names[:1], []), (names[1:], password)]
        for group, secret in groups:
            subprocess.run(command + ["-mhe=off", *secret, str(archive), *group],
                           cwd=directory, env=dict(os.environ, TZ="UTC", LC_ALL="C"), check=True)
        # Independent CLI verification of every member, outside measurement.
        for index, name in enumerate(names):
            result = subprocess.run([
                args.sevenzip, "x", "-so", *password, str(archive), name,
            ], check=True, capture_output=True)
            assert result.stdout == payload(4100 + index)
    data = archive.read_bytes()
    print(archive.name, len(data), hashlib.sha256(data).hexdigest())


if __name__ == "__main__":
    main()
