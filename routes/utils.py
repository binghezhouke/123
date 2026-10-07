"""
Shared route utilities
"""
from flask import g, current_app
from api import Pan123Client


def get_client() -> Pan123Client:
    """Get or create the Pan123Client for the current request context."""
    if "client" not in g:
        g.client = Pan123Client.from_app_config(current_app.config["PAN123_CONFIG"])
    return g.client


def folder_breadcrumbs(client, parent_id, refresh=False):
    """Return the known folder path for ``parent_id``, excluding the root.

    File metadata is fetched from the leaf toward the root, then reversed for
    display. A missing folder or API error leaves the portion already found
    available to the caller.
    """
    try:
        current_id = int(parent_id or 0)
    except (TypeError, ValueError):
        return []
    if current_id == 0:
        return []

    path = []
    visited = set()
    max_depth = 100
    while current_id not in (0, None) and len(path) < max_depth:
        if current_id in visited:
            current_app.logger.warning("Folder breadcrumb cycle detected at %s", current_id)
            break
        visited.add(current_id)
        try:
            if refresh and hasattr(client, 'clear_file_cache'):
                client.clear_file_cache(current_id)
            info = client.get_file_info_single(current_id, use_cache=not refresh)
            if not info:
                current_app.logger.warning("Folder breadcrumb metadata missing for %s", current_id)
                break
            if isinstance(info, dict):
                name = info.get("filename") or info.get("name")
                file_id = info.get("fileId", info.get("file_id", current_id))
                parent = info.get("parentFileId", info.get("parent_file_id", 0))
            else:
                name = getattr(info, "filename", None) or getattr(info, "name", None)
                file_id = getattr(info, "file_id", current_id)
                parent = getattr(info, "parent_file_id", 0)
            if not name:
                current_app.logger.warning("Folder breadcrumb name missing for %s", current_id)
                break
            path.append({"name": name, "file_id": file_id})
            current_id = int(parent or 0)
        except Exception as exc:
            current_app.logger.warning("Unable to load folder breadcrumb %s: %s", current_id, exc)
            break

    if len(path) == max_depth and current_id not in (0, None):
        current_app.logger.warning("Folder breadcrumb depth limit reached")
    path.reverse()
    return path
