"""Small, opt-in signature probe; never invoked while listing files."""
import hashlib
import json

from .zip_preview import RangeReader

SESSION_KEY = 'detected_archives'


def detect_archive(url):
    with RangeReader(url) as source:
        header = source.read(min(8, source.size))
    if header.startswith(b'7z\xbc\xaf\x27\x1c'):
        return '.7z'
    if header.startswith((b'Rar!\x1a\x07\x00', b'Rar!\x1a\x07\x01\x00')):
        return '.rar'
    if header.startswith((b'PK\x03\x04', b'PK\x05\x06', b'PK\x06\x06')):
        return '.zip'
    return None


def fingerprint(file):
    metadata = [file.filename, file.size, file.get('etag'), file.get('updateAt')]
    return hashlib.sha256(json.dumps(metadata, ensure_ascii=False).encode()).hexdigest()[:16]


def detected_kind(session, file):
    value = session.get(SESSION_KEY, {}).get(str(file.file_id))
    if value and value[0] in ('.zip', '.7z', '.rar') and value[1] == fingerprint(file):
        return value[0]
    return None


def remember_kind(session, file, kind):
    values = dict(session.get(SESSION_KEY, {}))
    values.pop(str(file.file_id), None)
    if kind:
        values[str(file.file_id)] = [kind, fingerprint(file)]
    # Keep the signed session cookie bounded; no signed URLs or passwords here.
    session[SESSION_KEY] = dict(list(values.items())[-24:])
