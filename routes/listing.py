"""Directory-wide ordering shared by HTML pagination and image navigation."""
import re
import secrets
from pathlib import PurePosixPath

from api import Pan123APIError

IMAGE_SUFFIXES = {'.png', '.jpg', '.jpeg', '.gif', '.webp', '.bmp'}
VIDEO_SUFFIXES = {'.mp4', '.m4v', '.webm', '.mov'}


def natural_key(name):
    return tuple((1, int(part)) if part.isdigit() else (0, part.casefold())
                 for part in re.split(r'(\d+)', name))


def options(args):
    sort = args.get('sort', 'original')
    direction = args.get('direction', 'asc')
    kind = args.get('kind', 'all')
    return (sort if sort in ('original', 'name', 'size') else 'original',
            direction if direction in ('asc', 'desc') else 'asc',
            kind if kind in ('all', 'image', 'video') else 'all')


def matches(filename, kind):
    suffix = PurePosixPath(filename).suffix.lower()
    return kind == 'all' or suffix in (IMAGE_SUFFIXES if kind == 'image' else VIDEO_SUFFIXES)


def directory_snapshot(client, cache, parent_id, token=None):
    if token:
        entries = cache.get((parent_id, 'snapshot', token))
        if entries is None:
            raise Pan123APIError('目录快照已过期，请刷新目录后重试')
        return entries, token
    cached = cache.get((parent_id, 'snapshot-current'))
    if cached:
        entries = cache.get((parent_id, 'snapshot', cached))
        if entries is not None:
            return entries, cached
    entries, seen, cursors = [], set(), set()
    cursor = None
    while True:
        page, following = client.list_files(parent_id=parent_id, limit=100, last_file_id=cursor)
        for file in page:
            if file.file_id not in seen:
                entries.append(file)
                seen.add(file.file_id)
        if following in (None, -1, 0):
            break
        if following in cursors or len(cursors) >= 1000:
            raise Pan123APIError('目录分页没有前进，请稍后重试')
        cursors.add(following)
        cursor = following
        if len(entries) > 50000:
            raise Pan123APIError('目录过大，暂时无法完成全量排序；请选择原始顺序浏览')
    token = secrets.token_urlsafe(12)
    cache.put((parent_id, 'snapshot', token), entries)
    cache.put((parent_id, 'snapshot-current'), token)
    return entries, token


def order_files(entries, sort, direction, kind):
    result = [f for f in entries if f.is_folder or matches(f.filename, kind)]
    if sort != 'original':
        result.sort(key=lambda f: ((f.size if sort == 'size' else natural_key(f.filename)),
                                   natural_key(f.filename), f.file_id), reverse=direction == 'desc')
        result.sort(key=lambda f: not f.is_folder)
    return result
