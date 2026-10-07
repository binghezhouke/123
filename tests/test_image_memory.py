"""Public disk reader keeps concurrent decompression buffers bounded."""
from concurrent.futures import ThreadPoolExecutor
import tracemalloc

from api.archive_preview import read_archive_member_file
from api.zip_preview import open_remote_zip
import test_zip_preview

remote = test_zip_preview.remote


def test_concurrent_large_image_readers_do_not_buffer_full_outputs(remote):
    remote.data = test_zip_preview.make_zip([('large.bmp', b'x' * (64 * 1024 * 1024))])

    def read_image():
        with open_remote_zip('https://fixture.test/large.zip') as (archive, source):
            with read_archive_member_file(archive, source, archive.infolist()[0]) as output:
                output.seek(0, 2)
                assert output.tell() == 64 * 1024 * 1024
                output.seek(-1, 2)
                assert output.read() == b'x'

    tracemalloc.start()
    try:
        with ThreadPoolExecutor(max_workers=2) as executor:
            list(executor.map(lambda _: read_image(), range(2)))
        _, peak = tracemalloc.get_traced_memory()
        assert peak < 32 * 1024 * 1024, f'Python allocation peak: {peak / 1024**2:.1f} MiB'
    finally:
        tracemalloc.stop()
