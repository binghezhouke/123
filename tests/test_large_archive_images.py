"""Images bypass the member cap; other types and integrity checks keep their limits."""
import io
import struct
import zipfile

import py7zr
import pyzipper
import pytest

import test_split_archive
import test_zip_preview
from api.archive_preview import BoundedWriter, open_archive, read_archive_member
from api.zip_preview import FILE_LIMIT, INDEX_LIMIT, ZipPreviewError

remote = test_zip_preview.remote
browser = test_zip_preview.browser
transport = test_split_archive.transport


@pytest.fixture(scope='module')
def large_bmp():
    # Valid uncompressed 24-bit BMP, 34.3 MiB, generated without external media.
    width, height = 4000, 3000
    size = 54 + width * height * 3
    return struct.pack('<2sIHHI', b'BM', size, 0, 0, 54) + struct.pack(
        '<IiiHHIIiiII', 40, width, height, 1, 24, 0, size - 54, 0, 0, 0, 0
    ) + b'\x80' * (size - 54)


@pytest.mark.parametrize('compression', [zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED])
def test_large_image_is_listed_previewed_and_downloaded(browser, remote, large_bmp, compression):
    remote.data = test_zip_preview.make_zip([('large.BMP', large_bmp)], compression)
    listing = browser.get('/file/1/zip')
    assert b'class="btn btn-sm btn-primary zip-image"' in listing.data
    for suffix in ['', '?download=1']:
        result = browser.get('/file/1/zip/member/0' + suffix)
        assert result.status_code == 200
        assert result.mimetype == 'image/bmp'
        assert result.data == large_bmp
    assert max(end - start + 1 for start, end in remote.ranges) <= INDEX_LIMIT


def test_large_non_image_keeps_limit(browser, remote, large_bmp):
    remote.data = test_zip_preview.make_zip([('large.bin', large_bmp)])
    assert browser.get('/file/1/zip/member/0').status_code == 400


def test_large_encrypted_zip_image_reads_without_whole_member_prefetch(remote, large_bmp):
    data = io.BytesIO()
    with pyzipper.AESZipFile(data, 'w', compression=zipfile.ZIP_STORED, encryption=pyzipper.WZ_AES) as archive:
        archive.setpassword(b'test')
        archive.writestr('large.bmp', large_bmp)
    remote.data = data.getvalue()
    with open_archive('https://download.test/image', '.zip', password='test') as (archive, source):
        assert read_archive_member(archive, source, archive.infolist()[0], max_size=None) == large_bmp
    assert max(end - start + 1 for start, end in remote.ranges) <= INDEX_LIMIT


@pytest.mark.parametrize('password', [None, 'test'])
def test_large_7z_image_bypasses_writer_and_library_caps(remote, large_bmp, password):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, 'w', password=password) as archive:
        archive.writestr(large_bmp, 'large.bmp')
    remote.data = data.getvalue()
    with open_archive('https://download.test/image', '.7z', password=password) as (archive, source):
        entry = archive.infolist()[0]
        assert entry.file_size > FILE_LIMIT
        assert read_archive_member(archive, source, entry, max_size=None) == large_bmp


def test_unlimited_image_path_still_checks_declared_output_size():
    writer = BoundedWriter(float('inf'), max_size=3)
    with pytest.raises(ZipPreviewError):
        writer.write(b'oversized')


def test_large_image_in_split_7z_crosses_volume_boundaries(transport, large_bmp):
    data = io.BytesIO()
    with py7zr.SevenZipFile(data, 'w', filters=[{'id': py7zr.FILTER_COPY}]) as archive:
        archive.writestr(large_bmp, 'large.bmp')
    volumes = test_split_archive.make_volumes(transport, data.getvalue(), 4 * 1024 * 1024)
    with open_archive(volumes, '.7z', resolve_part=lambda i: (f'https://part/{i}', 'api')) as (archive, source):
        assert read_archive_member(archive, source, archive.infolist()[0], max_size=None) == large_bmp
