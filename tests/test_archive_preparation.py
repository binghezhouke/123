"""Focused regression coverage for disk-backed extraction and nested prep jobs."""

import io
import json
import os
import re
import threading
import time
import tracemalloc
from concurrent.futures import ThreadPoolExecutor
import zipfile
from types import SimpleNamespace

import py7zr
import pyzipper
import pytest

import test_zip_preview
from api.archive_jobs import ArchiveJobError, ArchivePreparationJobs
from api.archive_preview import open_archive, read_archive_member_file
from api.models import File
from api.split_archive import Part
from api.zip_preview import INDEX_LIMIT, open_remote_zip


remote = test_zip_preview.remote


def test_disk_member_reader_is_seekable_bounded_and_validates_crc(remote):
    payload = b"x" * (96 * 1024 * 1024)
    remote.data = test_zip_preview.make_zip([("large.png", payload), ("next.txt", b"ok")], zipfile.ZIP_STORED)
    tracemalloc.start()
    baseline, _ = tracemalloc.get_traced_memory()
    with open_remote_zip("https://download.test/a.zip") as (archive, source):
        member_file = read_archive_member_file(archive, source, archive.infolist()[0])
        try:
            assert member_file.read(16) == payload[:16]
            member_file.seek(-16, 2)
            assert member_file.read() == payload[-16:]
        finally:
            member_file.close()
    _, peak = tracemalloc.get_traced_memory()
    tracemalloc.stop()
    assert peak - baseline < 40 * 1024 * 1024
    assert max(end - start + 1 for start, end in remote.ranges) <= INDEX_LIMIT

    def extract_to_disk():
        with open_remote_zip("https://download.test/a.zip") as (archive, source):
            member_file = read_archive_member_file(archive, source, archive.infolist()[0])
            try:
                member_file.seek(0, 2)
                return member_file.tell()
            finally:
                member_file.close()

    tracemalloc.start()
    baseline, _ = tracemalloc.get_traced_memory()
    with ThreadPoolExecutor(max_workers=2) as pool:
        assert list(pool.map(lambda _: extract_to_disk(), range(2))) == [len(payload), len(payload)]
    _, concurrent_peak = tracemalloc.get_traced_memory()
    tracemalloc.stop()
    assert concurrent_peak - baseline < 72 * 1024 * 1024

    damaged = bytearray(test_zip_preview.make_zip([("image.png", b"not the original")], zipfile.ZIP_STORED))
    with zipfile.ZipFile(io.BytesIO(damaged)) as local:
        info = local.infolist()[0]
    data_offset = info.header_offset + 30 + len(info.filename.encode()) + len(info.extra)
    damaged[data_offset] ^= 0x01
    remote.data = bytes(damaged)
    with open_remote_zip("https://download.test/b.zip") as (archive, source):
        with pytest.raises(zipfile.BadZipFile):
            read_archive_member_file(archive, source, archive.infolist()[0])


def test_disk_member_reader_honors_cancellation(remote):
    remote.data = test_zip_preview.make_zip([("large.png", b"x" * (2 * 1024 * 1024))], zipfile.ZIP_STORED)
    with open_remote_zip("https://download.test/a.zip") as (archive, source):
        source.cancel_event = threading.Event()
        source.cancel_event.set()
        with pytest.raises(InterruptedError):
            read_archive_member_file(archive, source, archive.infolist()[0])

    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w") as archive:
        archive.writestr(b"payload", "image.png")
    remote.data = data.getvalue()
    with open_archive("https://download.test/a.7z", ".7z") as (archive, source):
        source.cancel_event = threading.Event()
        source.cancel_event.set()
        with pytest.raises(InterruptedError):
            read_archive_member_file(archive, source, archive.infolist()[0])


def test_nested_zip_and_7z_extract_multiple_members_to_disk(tmp_path):
    class Jobs:
        def reserve_staging(self, record, amount):
            pass

        def set_phase(self, *args, **kwargs):
            pass

    record = {"cancel": threading.Event()}
    zippath = tmp_path / "inner.zip"
    with zipfile.ZipFile(zippath, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("dir/one.txt", "one")
        archive.writestr("two.txt", "two")
    from api.nested_archive import extract_nested_zip, extract_nested_7z
    extracted = extract_nested_zip(zippath, tmp_path / "zip-out", None, record, Jobs())
    assert (extracted / "dir/one.txt").read_text() == "one"
    assert (extracted / "two.txt").read_text() == "two"

    source = tmp_path / "seven-source"
    source.mkdir()
    (source / "one.txt").write_text("first")
    (source / "two.txt").write_text("second")
    sevenpath = tmp_path / "inner.7zz"
    with py7zr.SevenZipFile(sevenpath, "w") as archive:
        archive.write(source / "one.txt", "one.txt")
        archive.write(source / "two.txt", "two.txt")
    extracted = extract_nested_7z(sevenpath, tmp_path / "7z-out", None, record, Jobs())
    assert (extracted / "one.txt").read_text() == "first"
    assert (extracted / "two.txt").read_text() == "second"


@pytest.fixture
def nested_browser(tmp_path, monkeypatch, remote):
    from app import create_app

    config = tmp_path / "config.json"
    config.write_text(json.dumps({"CLIENT_ID": "id", "CLIENT_SECRET": "secret", "SECRET_KEY": "test",
                                  "ARCHIVE_PREP_CACHE_BYTES": 128 * 1024**2}))
    fake_client = SimpleNamespace(
        get_file_info_single=lambda *a, **kw: File({"fileId": 1, "filename": "outer.zip", "type": 0, "size": 1000,
                                                    "etag": "outer-v1", "updateAt": "today", "parentFileId": 0}),
        get_final_download_url=lambda *a, **kw: ("https://download.test/outer.zip", "api"),
    )
    monkeypatch.setattr("routes.zip_browser.get_client", lambda: fake_client)
    monkeypatch.setattr("routes.nested_archive.get_client", lambda: fake_client)
    app = create_app(str(config))
    app.config["TESTING"] = True
    yield app.test_client()
    app.extensions["archive_preparation_jobs"].close()


def _start_nested(client, member_id=0, password=None):
    listing = client.get("/file/1/zip")
    link = re.search(rb"nested/\d+\?v=([a-f0-9]+)", listing.data)
    assert link
    version = link.group(1).decode()
    page = client.get(f"/file/1/zip/nested/{member_id}?v={version}")
    csrf = re.search(rb'name="csrf_token" value="([^"]+)"', page.data)[1].decode()
    response = client.post(f"/file/1/zip/nested/{member_id}?v={version}", data={
        "csrf_token": csrf, "inner_password": password or "", "outer_password": "",
        "v": version,
    })
    job_id = response.headers["Location"].rsplit("/", 1)[-1]
    return job_id


def _wait_job(client, job_id):
    for _ in range(200):
        result = client.get(f"/api/archive-jobs/{job_id}")
        if result.status_code != 200:
            raise AssertionError(result.get_data(as_text=True))
        state = result.get_json()
        if state["state"] not in ("queued", "running"):
            return state
        time.sleep(0.025)
    raise AssertionError("archive preparation did not finish")


@pytest.mark.parametrize("inner_kind", ["zip", "7zz"])
def test_nested_archive_background_job_password_progress_browse_and_session_isolation(nested_browser, remote, inner_kind):
    inner = io.BytesIO()
    if inner_kind == "7zz":
        with py7zr.SevenZipFile(inner, "w", password="secret") as archive:
            archive.writestr(b"image-data", "pictures/a.png")
            archive.writestr(b"text", "readme.txt")
    else:
        with pyzipper.AESZipFile(inner, "w", compression=zipfile.ZIP_DEFLATED, encryption=pyzipper.WZ_AES) as archive:
            archive.setpassword(b"secret")
            archive.writestr("pictures/a.png", b"image-data")
            archive.writestr("readme.txt", b"text")
    remote.data = test_zip_preview.make_zip([(f"bundle.{inner_kind}", inner.getvalue())], zipfile.ZIP_STORED)
    job_id = _start_nested(nested_browser, password="secret")
    state = _wait_job(nested_browser, job_id)
    assert state["state"] == "complete"
    assert state["processed"] > 0 and state["total"] == state["processed"]
    browser = nested_browser.get(f"/archive-jobs/{job_id}/browse")
    assert b"pictures/" in browser.data and b"readme.txt" in browser.data
    image_dir = nested_browser.get(f"/archive-jobs/{job_id}/browse?path=pictures")
    assert b"zip-image" in image_dir.data

    other_session = nested_browser.application.test_client()
    assert other_session.get(f"/api/archive-jobs/{job_id}").status_code == 404
    second_id = _start_nested(other_session, password="secret")
    assert second_id != job_id
    assert _wait_job(other_session, second_id)["state"] == "complete"


def test_nested_password_cache_is_session_scoped_and_cancel_cleans_staging(tmp_path):
    jobs = ArchivePreparationJobs(root=tmp_path / "cache", max_bytes=1024)
    entered, release = threading.Event(), threading.Event()
    def worker(staging, record):
        output = staging / "contents"
        output.mkdir()
        entered.set()
        while not release.wait(0.01):
            if record["cancel"].is_set():
                raise InterruptedError("cancelled")
        return output
    one = jobs.start(("same", "encrypted"), "session-a", worker)
    assert entered.wait(2)
    assert jobs.status(one["id"], "session-b") is None
    assert jobs.cancel(one["id"], "session-a")
    release.set()
    for _ in range(100):
        state = jobs.status(one["id"], "session-a")
        if state["state"] == "cancelled":
            break
        time.sleep(0.01)
    assert state["state"] == "cancelled"
    assert not list((tmp_path / "cache").glob("job-*"))
    with pytest.raises(ArchiveJobError, match="预算不足"):
        jobs.reserve_staging({"id": "bogus"}, 2048)
    jobs.close()


def test_job_cache_removes_only_marked_directories_from_dead_processes(tmp_path):
    parent = tmp_path / "archive-cache-root"
    parent.mkdir()
    stale = parent / "123-archive-cache-stale"
    stale.mkdir()
    (stale / "owner.json").write_text(json.dumps({"pid": 999999999, "uid": getattr(os, "getuid", lambda: None)(),
                                                   "directory": stale.name}))
    (stale / "partial").write_text("orphaned")
    unrelated = parent / "123-archive-cache-unmarked"
    unrelated.mkdir()
    (unrelated / "partial").write_text("keep")
    jobs = ArchivePreparationJobs(root=parent)
    assert not stale.exists()
    assert (unrelated / "partial").exists()
    own_root = jobs.root
    jobs.close()
    jobs.close()
    assert not own_root.exists()


def test_nested_http_cancel_cleans_incomplete_job(nested_browser, remote, monkeypatch):
    remote.data = test_zip_preview.make_zip([("inner.zip", test_zip_preview.make_zip([("x.txt", b"x")]))])
    jobs = nested_browser.application.extensions["archive_preparation_jobs"]
    original_start = jobs.start
    entered = threading.Event()

    def start(cache_key, owner, worker):
        def blocked(staging, record):
            (staging / "partial").write_bytes(b"partial")
            entered.set()
            while not record["cancel"].wait(0.01):
                pass
            raise InterruptedError("cancelled")
        return original_start(cache_key, owner, blocked)

    monkeypatch.setattr(jobs, "start", start)
    job_id = _start_nested(nested_browser)
    assert entered.wait(2)
    page = nested_browser.get(f"/archive-jobs/{job_id}")
    csrf = re.search(rb'name="csrf_token" value="([^"]+)"', page.data)[1].decode()
    cancelled = nested_browser.post(f"/api/archive-jobs/{job_id}", data={"csrf_token": csrf})
    assert cancelled.status_code == 200
    state = _wait_job(nested_browser, job_id)
    assert state["state"] == "cancelled"
    assert not list(jobs.root.glob("job-*"))


def test_nested_prepare_from_split_7z_outer(tmp_path, monkeypatch):
    from app import create_app

    inner = test_zip_preview.make_zip([("message.txt", b"split works")])
    archive_data = io.BytesIO()
    with py7zr.SevenZipFile(archive_data, "w") as archive:
        archive.writestr(inner, "inner.zip")
    payload = archive_data.getvalue()
    part_size = 80
    parts = []
    payloads = {}
    rows = []
    for start in range(0, len(payload), part_size):
        index = len(parts) + 1
        part_data = payload[start:start + part_size]
        part = Part(index, f"bundle.7z.{index:03}", len(part_data), f"etag-{index}")
        parts.append(part)
        payloads[f"https://split/{index}"] = part_data
        rows.append(File({"fileId": index, "filename": part.name, "size": part.size, "type": 0,
                          "parentFileId": 5, "etag": part.etag}))

    class Response:
        status_code = 206

        def __init__(self, url, start, end):
            data = payloads[url]
            self.headers = {"Content-Range": f"bytes {start}-{end}/{len(data)}", "ETag": '"part"'}
            self.data = data[start:end + 1]

        def __enter__(self): return self
        def __exit__(self, *args): return None
        def iter_content(self, size):
            for offset in range(0, len(self.data), size):
                yield self.data[offset:offset + size]

    def get(self, url, headers, **kwargs):
        start, end = map(int, re.findall(r"\d+", headers["Range"]))
        return Response(url, start, end)

    monkeypatch.setattr("requests.Session.get", get)

    class FakeClient:
        def get_file_info_single(self, *args, **kwargs): return rows[0]
        def list_files(self, **kwargs): return rows, -1
        def get_final_download_url(self, file_id, **kwargs): return f"https://split/{file_id}", "api"
        def __enter__(self): return self
        def __exit__(self, *args): return None

    fake = FakeClient()
    monkeypatch.setattr("routes.zip_browser.get_client", lambda: fake)
    monkeypatch.setattr("routes.nested_archive.get_client", lambda: fake)
    monkeypatch.setattr("routes.nested_archive.Pan123Client.from_app_config", classmethod(lambda cls, config: fake))
    config = tmp_path / "config.json"
    config.write_text(json.dumps({"CLIENT_ID": "id", "CLIENT_SECRET": "secret", "SECRET_KEY": "test",
                                  "ARCHIVE_PREP_CACHE_BYTES": 128 * 1024**2}))
    app = create_app(str(config))
    app.config["TESTING"] = True
    client = app.test_client()
    try:
        job_id = _start_nested(client)
        assert _wait_job(client, job_id)["state"] == "complete"
        listing = client.get(f"/archive-jobs/{job_id}/browse")
        assert b"message.txt" in listing.data
        assert client.get(f"/archive-jobs/{job_id}/member/message.txt").data == b"split works"
    finally:
        app.extensions["archive_preparation_jobs"].close()


def test_process_shutdown_cancels_pending_archive_preparation(tmp_path):
    """Normal interpreter exit reaches cancellation instead of joining a stuck worker first."""
    import subprocess
    import sys
    script = '''
import atexit, sys, threading
from api.archive_jobs import ArchivePreparationJobs
jobs = ArchivePreparationJobs(root=sys.argv[1])
atexit.register(jobs.close)
entered = threading.Event()
def prepare(staging, record):
    entered.set()
    record['cancel'].wait(60)
    raise InterruptedError('cancelled')
jobs.start(('archive', 1), 'session', prepare)
assert entered.wait(2)
'''
    completed = subprocess.run([sys.executable, '-c', script, str(tmp_path / 'cache')],
                               timeout=5, capture_output=True, text=True)
    assert completed.returncode == 0, completed.stderr
    assert not list((tmp_path / 'cache').glob('123-archive-cache-*'))


def test_staging_disk_failure_becomes_visible_failed_job(nested_browser, remote, monkeypatch):
    remote.data = test_zip_preview.make_zip([('inner.zip', test_zip_preview.make_zip([('one.txt', b'one')]))])
    import tempfile
    original = tempfile.mkdtemp
    def create_directory(*args, **kwargs):
        if kwargs.get('prefix') == 'job-':
            raise OSError(28, 'disk full')
        return original(*args, **kwargs)
    monkeypatch.setattr('api.archive_jobs.tempfile.mkdtemp', create_directory)
    job_id = _start_nested(nested_browser)
    state = _wait_job(nested_browser, job_id)
    assert state['state'] == 'failed'
    assert '磁盘空间' in state['error']
