"""Bounded in-memory store for archive passwords, isolated by browser session."""

from collections import OrderedDict
from threading import Lock
from time import monotonic


class ArchivePasswordVault:
    def __init__(self, ttl=900, capacity=256):
        self.ttl = ttl
        self.capacity = capacity
        self._values = OrderedDict()
        self._lock = Lock()

    def _purge(self, now):
        for key, (expires, _) in list(self._values.items()):
            if expires <= now:
                self._values.pop(key, None)

    def get(self, session_token, archive_id):
        key = (session_token, archive_id)
        now = monotonic()
        with self._lock:
            self._purge(now)
            value = self._values.get(key)
            if value is None:
                return None
            self._values.move_to_end(key)
            return value[1]

    def set(self, session_token, archive_id, password):
        key = (session_token, archive_id)
        now = monotonic()
        with self._lock:
            self._purge(now)
            self._values.pop(key, None)
            self._values[key] = (now + self.ttl, password)
            while len(self._values) > self.capacity:
                self._values.popitem(last=False)

    def clear(self, session_token, archive_id):
        with self._lock:
            self._values.pop((session_token, archive_id), None)
