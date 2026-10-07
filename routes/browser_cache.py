"""Small per-application page cache, shared across request-scoped API clients."""

from collections import OrderedDict
from threading import Lock
from time import monotonic


class PageCache:
    def __init__(self, ttl=30, capacity=128):
        self.ttl, self.capacity = ttl, capacity
        self.entries = OrderedDict()
        self.lock = Lock()

    def get(self, key):
        with self.lock:
            item = self.entries.get(key)
            if item and monotonic() - item[0] < self.ttl:
                self.entries.move_to_end(key)
                return item[1]
            self.entries.pop(key, None)
            return None

    def put(self, key, value):
        with self.lock:
            self.entries[key] = (monotonic(), value)
            self.entries.move_to_end(key)
            while len(self.entries) > self.capacity:
                self.entries.popitem(last=False)

    def invalidate_directory(self, parent_id):
        with self.lock:
            for key in list(self.entries):
                if key[0] == parent_id:
                    del self.entries[key]
