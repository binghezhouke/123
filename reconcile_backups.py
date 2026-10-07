"""Compare a local manifest with remote /backups and write a reconciliation report."""

import json
import sys
from pathlib import Path
from api import Pan123Client


def main():
    manifest = Path(sys.argv[1])
    out = Path(sys.argv[2])
    local = json.loads(manifest.read_text())
    by_path = {x["path"]: (x["size"], (x.get("md5") or x.get("etag", "")).lower()) for x in local["files"]}
    remote = {}
    with Pan123Client() as c:

        def walk(parent, prefix=""):
            rows, _ = c.file_service.list_files(
                parent_id=parent, limit=100, auto_fetch_all=True, use_cache=False, max_pages=200
            )
            for x in rows:
                name = x.get("filename") or x.get("name", "")
                typ = x.get("type", x.get("category"))
                fid = x.get("fileId") or x.get("file_id")
                p = f"{prefix}/{name}" if prefix else name
                if typ in (1, "1", "dir", "directory") or x.get("type") == 1:
                    walk(fid, p)
                else:
                    remote[p.lstrip("/")] = (int(x.get("size") or 0), str(x.get("etag") or "").lower())

        roots, _ = c.file_service.list_files(parent_id=0, limit=100, auto_fetch_all=True, use_cache=False, max_pages=20)
        bid = next((x.get("fileId") for x in roots if (x.get("filename") or x.get("name")) == "backups"), None)
        if bid is None:
            raise SystemExit("remote /backups not found")
        walk(bid)
    uploaded = []
    missing = []
    mismatch = []
    for p, (size, md5) in by_path.items():
        rp = p.lstrip("/")
        got = remote.get(rp)
        if got and got[0] == size and got[1] == md5:
            uploaded.append(p)
        elif got:
            mismatch.append({"path": p, "local": {"size": size, "md5": md5}, "remote": {"size": got[0], "md5": got[1]}})
        else:
            missing.append(p)
    out.write_text(
        json.dumps(
            {"uploaded": uploaded, "missing": missing, "mismatch": mismatch, "remote_file_count": len(remote)},
            ensure_ascii=False,
            indent=2,
        )
    )
    print(f"remote={len(remote)} uploaded={len(uploaded)} missing={len(missing)} mismatch={len(mismatch)}")


if __name__ == "__main__":
    main()
