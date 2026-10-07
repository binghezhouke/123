"""HTTP byte ranges over a stored ZIP video; streaming owns its reader lifetime."""

import time
import re
from urllib.parse import quote

from flask import Response, request
from werkzeug.http import parse_range_header

from api.zip_preview import RangeReader, RangeSnapshot

STREAM_CHUNK = 256 * 1024


def video_response(member, filename, mimetype, download=False, allow_range=True):
    headers = {
        "Accept-Ranges": "bytes" if allow_range else "none",
        "Content-Type": mimetype,
        "Content-Disposition": f"{'attachment' if download else 'inline'}; filename*=UTF-8''{quote(filename, safe='')}",
        "X-Content-Type-Options": "nosniff",
        "Cache-Control": "private, no-store",
    }
    start, stop = 0, member.size
    status = 200
    # If-Range does not match a media validator: return the full representation.
    # Archive validators refer to the ZIP, not this virtual resource.
    range_header = (
        request.headers.get("Range")
        if allow_range and request.method == "GET" and not request.headers.get("If-Range")
        else None
    )
    if range_header:
        parsed = parse_range_header(range_header)
        interval = parsed.range_for_length(member.size) if parsed and parsed.units == "bytes" else None
        if interval is None or re.fullmatch(r"bytes=-0+", range_header.strip()):
            headers["Content-Range"] = f"bytes */{member.size}"
            return Response(status=416, headers=headers)
        start, stop = interval
        status = 206
        headers["Content-Range"] = f"bytes {start}-{stop - 1}/{member.size}"
    headers["Content-Length"] = str(stop - start)
    if request.method == "HEAD" or start == stop:
        return Response(status=status, headers=headers)

    # Never retain the archive parser: ArchiveCache closes it when the route returns.
    reader = RangeReader(member.url, snapshot=RangeSnapshot(member.archive_size, member.validator, ()))
    reader.seek(member.offset + start)
    end = member.offset + stop

    def read_chunk():
        # Bound memory and each upstream request, rather than the complete video size.
        reader.remaining = STREAM_CHUNK
        reader.deadline = time.monotonic() + 60
        return reader.read(min(STREAM_CHUNK, end - reader.tell()))

    try:
        # Eager first read lets the archive cache refresh expired URLs before sending headers.
        first = read_chunk()
    except BaseException:
        reader.close()
        raise

    def chunks():
        try:
            yield first
            while reader.tell() < end:
                yield read_chunk()
        finally:
            reader.close()

    response = Response(chunks(), status=status, headers=headers)
    response.call_on_close(reader.close)
    return response
