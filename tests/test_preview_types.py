from routes.preview_types import classify, mimetype_for


def test_shared_preview_classification_covers_directory_and_archive_media():
    assert classify("photo.JPG") == "image"
    assert classify("movie.MKV") == "video"
    assert classify("voice.m4a") == "audio"
    assert classify("notes.txt") == "text"
    assert mimetype_for("movie.mkv") == "video/x-matroska"


def test_unknown_suffix_has_no_preview():
    assert classify("payload.bin") is None
    assert mimetype_for("payload.bin") is None
