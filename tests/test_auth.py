"""Token refresh synchronization tests without credentials or network access."""

import threading
import time

from api.auth import TokenManager


def test_concurrent_rejected_token_refreshes_are_coalesced(monkeypatch):
    manager = TokenManager("https://example.invalid", "id", "secret")
    manager._access_token = "old"
    manager._token_expires_at = time.time() + 3600
    calls = []

    def fetch():
        calls.append(True)
        time.sleep(0.02)
        manager._access_token = "new"
        manager._token_expires_at = time.time() + 3600

    monkeypatch.setattr(manager, "_fetch_new_token", fetch)
    barrier = threading.Barrier(8)
    results = []

    def refresh():
        barrier.wait()
        results.append(manager.refresh_if_current("old"))

    workers = [threading.Thread(target=refresh) for _ in range(8)]
    for worker in workers:
        worker.start()
    for worker in workers:
        worker.join()

    assert results == ["new"] * 8
    assert len(calls) == 1
