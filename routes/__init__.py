"""
Routes package for 123云盘 Flask application
"""
from .main import main_bp
from .api import api_bp
from .utils import get_client

__all__ = ['main_bp', 'api_bp', 'get_client']
