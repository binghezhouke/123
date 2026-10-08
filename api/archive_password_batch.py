"""Directory snapshots and eligibility for explicitly requested ZIP passwords."""

from collections import Counter
from time import monotonic, sleep

from .exceptions import ValidationError


def password_directory(client, parent_id):
    """Fetch every page, refusing partial/cyclic listings before any writes."""
    files, seen_ids, cursors = [], set(), set()
    cursor = None
    previous = 0
    while True:
        if previous:
            sleep(max(0, 0.2 - (monotonic() - previous)))
        previous = monotonic()
        page, following = client.list_files(
            parent_id=parent_id, limit=100, last_file_id=cursor, use_cache=False)
        for file in page:
            if str(file.trashed) in ("1", "True") or file.file_id in seen_ids:
                continue
            seen_ids.add(file.file_id)
            files.append(file)
        if len(files) > 50000:
            raise ValidationError("目录超过 50000 项，请在较小的目录中设置密码")
        if following in (None, -1, 0):
            return files
        if following in cursors or len(cursors) >= 1000:
            raise ValidationError("目录分页不完整，请刷新后重试")
        cursors.add(following)
        cursor = following


def zip_password_targets(files):
    names = Counter(file.filename for file in files)
    targets = []
    for file in files:
        if file.is_folder or not file.filename.lower().endswith(".zip"):
            continue
        reason = ("已存在同名密码文件或目录" if names[file.filename + ".pwd"] else
                  "存在同名 ZIP，无法唯一对应密码文件" if names[file.filename] > 1 else "")
        targets.append({"id": file.file_id, "name": file.filename,
                        "status": "skipped" if reason else "pending", "message": reason})
    return targets
