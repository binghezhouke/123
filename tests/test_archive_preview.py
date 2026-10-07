import io
import os
import shutil
import subprocess
from pathlib import Path

import py7zr
import pytest
import rarfile

from api.archive_preview import open_archive, read_archive_member
from api.zip_preview import ZipPreviewError
import test_zip_preview

remote_range = test_zip_preview.remote


@pytest.fixture
def remote(remote_range):
    return remote_range


def test_non_solid_7z_reads_member(remote):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w") as archive:
        archive.writestr(b"hello", "hello.txt")
    remote.data = data.getvalue()
    with open_archive("https://example.test/a", ".7z") as (archive, source):
        assert not archive.solid
        assert archive.infolist()[0].filename == "hello.txt"
        assert read_archive_member(archive, source, archive.infolist()[0]) == b"hello"


def test_solid_7z_lists_but_rejects_extraction(remote):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w") as archive:
        archive.writestr(b"a", "a.txt")
        archive.writestr(b"b", "b.txt")
    remote.data = data.getvalue()
    with open_archive("https://example.test/a", ".7z") as (archive, source):
        assert archive.solid
        assert len(archive.infolist()) == 2
        with pytest.raises(ZipPreviewError, match="固实"):
            read_archive_member(archive, source, archive.infolist()[1])


def test_non_solid_rar_extracts_real_compressed_member(remote):
    if not shutil.which("unrar"):
        pytest.skip("requires unrar")
    path = Path(__file__).parent / "fixtures/rar/rar5-crc.rar"
    remote.data = path.read_bytes()
    with rarfile.RarFile(path) as local:
        expected = {f.filename: local.read(f) for f in local.infolist() if not f.isdir()}
    with open_archive("https://example.test/a", ".rar") as (archive, source):
        assert not archive.solid
        for entry in archive.infolist():
            if not entry.is_dir():
                assert read_archive_member(archive, source, entry) == expected[entry.filename]


def test_solid_rar_lists_but_rejects_extraction(remote):
    remote.data = (Path(__file__).parent / "fixtures/rar/rar3-solid.rar").read_bytes()
    with open_archive("https://example.test/a", ".rar") as (archive, source):
        assert archive.solid
        with pytest.raises(ZipPreviewError, match="固实"):
            read_archive_member(archive, source, archive.infolist()[0])


def test_non_solid_7z_does_not_download_large_sibling(remote, tmp_path):
    if not shutil.which("7z"):
        pytest.skip("requires 7z to generate non-solid fixture")
    (tmp_path / "large.bin").write_bytes(os.urandom(3_000_000))
    (tmp_path / "small.txt").write_bytes(b"small")
    subprocess.run(
        ["7z", "a", "-ms=off", "sample.7z", "large.bin", "small.txt"],
        cwd=tmp_path,
        check=True,
        stdout=subprocess.DEVNULL,
    )
    remote.data = (tmp_path / "sample.7z").read_bytes()
    with open_archive("https://example.test/a", ".7z") as (archive, source):
        assert not archive.solid
        entry = next(e for e in archive.infolist() if e.filename == "small.txt")
        assert read_archive_member(archive, source, entry) == b"small"
    assert sum(end - start + 1 for start, end in remote.ranges) < 1_000_000


def test_encrypted_7z_header_returns_readable_error(remote):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w", password="fixture-only", header_encryption=True) as archive:
        archive.writestr(b"hello", "hello.txt")
    remote.data = data.getvalue()
    with pytest.raises(ZipPreviewError, match="加密"):
        with open_archive("https://example.test/a", ".7z"):
            pass


def test_empty_7z_lists_without_server_error(remote):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, "w"):
        pass
    remote.data = data.getvalue()
    with open_archive("https://example.test/a", ".7z") as (archive, _):
        assert archive.infolist() == []
