#!/usr/bin/env python3
"""Generate synthetic group-start fixtures with externally supplied RAR 6.24.

No archiver is bundled or downloaded. Use --rar PATH --out EMPTY_DIRECTORY.
"""
import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile

RAR_SHA256 = "71ab9088d575e438d243672fcc6d36d2f9a39f5a8d4d26e998129cea12dbf682"

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
    args = parser.parse_args()
    rar = args.rar.resolve()
    if hashlib.sha256(rar.read_bytes()).hexdigest() != RAR_SHA256:
        parser.error("expected official RAR 6.24 Linux x64 binary")
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    if any(out.glob("*.rar")):
        parser.error("output directory must contain no RAR files")
    env = dict(os.environ, TZ="UTC", LC_ALL="C")
    with tempfile.TemporaryDirectory(prefix="rar-groups-") as directory:
        for name in ("a.bin", "b.bin", "c.bin", "d.bin"):
            path = Path(directory) / name
            path.write_bytes(payload())
            path.chmod(0o644)
            os.utime(path, (946684800, 946684800))
        for version in (3, 5):
            for suffix, extra in (("two-groups", []), ("two-groups-multipart", ["-sv-", "-v16384b"]), ("two-groups-password", ["-ppassword"])):
                subprocess.run([str(rar), "a", "-cfg-", "-ma4" if version == 3 else "-ma5", "-m3", "-md256k", "-s2", "-mt1", "-ep", "-ts-", "-oi0", "-idq", *extra, str(out / f"rar{version}-{suffix}.rar"), "a.bin", "b.bin", "c.bin", "d.bin"], cwd=directory, env=env, check=True)
            for name in ("c.bin", "d.bin"):
                (Path(directory) / name).write_bytes(payload()[:4096])
                os.utime(Path(directory) / name, (946684800, 946684800))
            subprocess.run([str(rar), "a", "-cfg-", "-ma4" if version == 3 else "-ma5", "-m3", "-md256k", "-s2", "-mt1", "-ep", "-ts-", "-oi0", "-idq", str(out / f"rar{version}-group-budget.rar"), "a.bin", "b.bin", "c.bin", "d.bin"], cwd=directory, env=env, check=True)
            for name in ("c.bin", "d.bin"):
                (Path(directory) / name).write_bytes(payload())
                os.utime(Path(directory) / name, (946684800, 946684800))
        (Path(directory) / "empty.bin").touch()
        (Path(directory) / "folder").mkdir()
        for path in (Path(directory) / "empty.bin", Path(directory) / "folder"):
            path.chmod(0o755 if path.is_dir() else 0o644)
            os.utime(path, (946684800, 946684800))
        subprocess.run([str(rar), "a", "-cfg-", "-ma5", "-m3", "-md256k", "-mt1", "-ts-", "-oi0", "-idq", str(out / "rar5-group-boundaries.rar"), "folder", "empty.bin", "a.bin"], cwd=directory, env=env, check=True)
    for path in sorted(out.glob("*.rar")):
        print(path.name, path.stat().st_size, hashlib.sha256(path.read_bytes()).hexdigest())

if __name__ == "__main__":
    main()
