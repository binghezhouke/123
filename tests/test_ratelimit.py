"""TokenBucket 限速器测试（时间断言留了余量，避免机器负载导致偶发失败）"""

import threading
import time

import pytest

from api.ratelimit import TokenBucket


def test_rejects_non_positive_rate():
    with pytest.raises(ValueError):
        TokenBucket(0)


def test_burst_is_immediate():
    bucket = TokenBucket(rate=1, burst=5)

    start = time.monotonic()
    for _ in range(5):
        bucket.acquire()
    elapsed = time.monotonic() - start

    assert elapsed < 0.2, "突发额度内不应该等待"


def test_paces_after_burst_is_exhausted():
    bucket = TokenBucket(rate=20, burst=1)

    bucket.acquire()          # 用掉唯一一个令牌
    start = time.monotonic()
    bucket.acquire()
    bucket.acquire()
    elapsed = time.monotonic() - start

    # 20/s => 两个令牌约 0.1 秒
    assert elapsed >= 0.07, f"限速没生效，只用了 {elapsed:.3f}s"


def test_acquire_is_thread_safe():
    bucket = TokenBucket(rate=50, burst=1)
    done = []

    def worker():
        for _ in range(3):
            bucket.acquire()
            done.append(1)

    threads = [threading.Thread(target=worker) for _ in range(10)]
    start = time.monotonic()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.monotonic() - start

    assert len(done) == 30
    # 30 个请求、50/s、初始 1 个令牌 => 至少约 0.58 秒
    assert elapsed >= 0.4, f"并发下没有限住：{elapsed:.3f}s"
