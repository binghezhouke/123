import io
import shutil
import subprocess
from pathlib import Path

import py7zr
import pyzipper
import pytest

import test_zip_preview
from api.archive_cache import ArchiveCache
from api.archive_preview import open_archive, read_archive_member
from api.zip_preview import ArchivePasswordRequired

remote_range = test_zip_preview.remote


@pytest.fixture
def remote(remote_range):
    return remote_range


def encrypted_data(kind, header=False):
    output = io.BytesIO()
    if kind == ".zip":
        with pyzipper.AESZipFile(output, "w", compression=pyzipper.ZIP_DEFLATED, encryption=pyzipper.WZ_AES) as archive:
            archive.setpassword(b"fixture-only")
            archive.writestr("a.txt", b"hello encrypted world")
    else:
        with py7zr.SevenZipFile(output, "w", password="fixture-only", header_encryption=header) as archive:
            archive.writestr(b"hello encrypted world", "a.txt")
    return output.getvalue()


@pytest.mark.parametrize("kind,header", [(".zip", False), (".7z", False), (".7z", True)])
def test_encrypted_archive_roundtrip_and_wrong_password(remote, kind, header):
    remote.data = encrypted_data(kind, header)
    with open_archive("https://example.test/a", kind, password="fixture-only") as (archive, source):
        assert read_archive_member(archive, source, archive.infolist()[0]) == b"hello encrypted world"
    for password in (None, "incorrect"):
        with pytest.raises(ArchivePasswordRequired):
            with open_archive("https://example.test/a", kind, password=password) as (archive, source):
                read_archive_member(archive, source, archive.infolist()[0])


@pytest.mark.parametrize("kind,header", [(".zip", False), (".7z", True)])
def test_cached_encrypted_index_still_requires_each_callers_password(remote, kind, header):
    remote.data = encrypted_data(kind, header)
    cache = ArchiveCache()

    def read(archive, source):
        return read_archive_member(archive, source, archive.infolist()[0])

    def run(password):
        return cache.run("same-file", kind, lambda: ("https://example.test/a", "api"), read, password=password)

    assert run("fixture-only") == b"hello encrypted world"
    for password in (None, "incorrect"):
        with pytest.raises(ArchivePasswordRequired):
            run(password)
    assert run("fixture-only") == b"hello encrypted world"


def test_zipcrypto(remote, tmp_path):
    if not shutil.which("7z"):
        pytest.skip("requires 7z fixture generator")
    (tmp_path / "a.txt").write_bytes(b"zipcrypto")
    subprocess.run(
        ["7z", "a", "-tzip", "-pfixture-only", "test.zip", "a.txt"], cwd=tmp_path, check=True, stdout=subprocess.DEVNULL
    )
    remote.data = (tmp_path / "test.zip").read_bytes()
    with open_archive("https://example.test/a", ".zip", password="fixture-only") as (archive, source):
        assert read_archive_member(archive, source, archive.infolist()[0]) == b"zipcrypto"


@pytest.mark.parametrize("name", ["rar5-psw.rar", "rar5-hpsw.rar"])
def test_rar5_password(remote, name):
    if not shutil.which("unrar"):
        pytest.skip("requires unrar")
    remote.data = (Path(__file__).parent / "fixtures/rar" / name).read_bytes()
    with open_archive("https://example.test/a", ".rar", password="password") as (archive, source):
        entry = next(e for e in archive.infolist() if not e.is_dir())
        assert read_archive_member(archive, source, entry)
    with pytest.raises(ArchivePasswordRequired):
        with open_archive("https://example.test/a", ".rar", password="incorrect") as (archive, source):
            read_archive_member(archive, source, next(e for e in archive.infolist() if not e.is_dir()))


def test_large_encrypted_rar_rejected_before_whole_archive_read(remote):
    from api.zip_preview import FILE_LIMIT, ZipPreviewError

    remote.data = (Path(__file__).parent / "fixtures/rar/rar5-psw.rar").read_bytes()
    with open_archive("https://example.test/a", ".rar", password="password") as (archive, source):
        source.size = FILE_LIMIT + 1
        fetched = len(remote.ranges)
        with pytest.raises(ZipPreviewError, match="32 MiB"):
            read_archive_member(archive, source, next(e for e in archive.infolist() if not e.is_dir()))
        assert len(remote.ranges) == fetched
