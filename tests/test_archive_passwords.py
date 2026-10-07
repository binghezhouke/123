from api.archive_passwords import ArchivePasswordVault


def test_password_vault_isolates_sessions_and_expires(monkeypatch):
    now = [10.0]
    monkeypatch.setattr("api.archive_passwords.monotonic", lambda: now[0])
    vault = ArchivePasswordVault(ttl=5, capacity=2)
    vault.set("browser-a", "archive", "secret")
    assert vault.get("browser-a", "archive") == "secret"
    assert vault.get("browser-b", "archive") is None
    now[0] = 15.0
    assert vault.get("browser-a", "archive") is None


def test_password_vault_evicts_oldest_and_clears():
    vault = ArchivePasswordVault(capacity=2)
    vault.set("session", "one", "1")
    vault.set("session", "two", "2")
    vault.set("session", "three", "3")
    assert vault.get("session", "one") is None
    assert vault.get("session", "two") == "2"
    vault.clear("session", "two")
    assert vault.get("session", "two") is None
