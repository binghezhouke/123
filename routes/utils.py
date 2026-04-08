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
