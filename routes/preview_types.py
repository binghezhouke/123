"""Shared file preview classification for directory and archive views."""
from pathlib import PurePosixPath

TEXT_EXTENSIONS = {
    ".txt", ".md", ".log", ".json", ".csv", ".xml", ".html", ".css",
    ".js", ".py", ".yaml", ".yml", ".ini", ".toml", ".sh", ".sql", ".svg",
}
PREVIEW_TYPES = {
    ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
    ".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
    ".pdf": "application/pdf",
}
MEDIA_TYPES = {
    ".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
    ".mov": "video/quicktime", ".mkv": "video/x-matroska",
    ".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".m4a": "audio/mp4",
}


def suffix_for(name):
    return PurePosixPath(name).suffix.lower()


def classify(name):
    """Return the common preview kind used by file and archive pages."""
    suffix = suffix_for(name)
    if PREVIEW_TYPES.get(suffix, "").startswith("image/"):
        return "image"
    if suffix in MEDIA_TYPES and MEDIA_TYPES[suffix].startswith("video/"):
        return "video"
    if suffix in MEDIA_TYPES and MEDIA_TYPES[suffix].startswith("audio/"):
        return "audio"
    if suffix == ".pdf":
        return "pdf"
    if suffix in TEXT_EXTENSIONS:
        return "text"
    return None


def mimetype_for(name):
    suffix = suffix_for(name)
    return PREVIEW_TYPES.get(suffix) or MEDIA_TYPES.get(suffix)
