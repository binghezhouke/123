"""请求限速。

123 云盘的限流是账号级的：短时间内在飞的请求一多，就会返回 HTTP 200 +
业务码 1（"请慢一点"）。实测同一账号下同时并发 ≤8 个请求不报错，再多就开始被限，
而且加线程并不会提高吞吐（上限约 8 请求/秒），所以客户端这边按速率节流就够了。
"""

import threading
import time


class TokenBucket:
    """线程安全的令牌桶：限制长期平均速率，同时允许一定突发。"""

    def __init__(self, rate: float, burst: float = None):
        """
        :param rate: 每秒放行的请求数
        :param burst: 允许的瞬时突发量，默认等于 rate
        """
        if rate <= 0:
            raise ValueError("rate 必须大于 0")
        self.rate = float(rate)
        self.burst = float(burst if burst is not None else max(1.0, self.rate))
        self._tokens = self.burst
        self._updated = time.monotonic()
        self._lock = threading.Lock()

    def acquire(self, tokens: float = 1.0) -> float:
        """取令牌，不够就阻塞等待；返回实际等待的秒数。"""
        waited = 0.0
        while True:
            with self._lock:
                now = time.monotonic()
                elapsed = now - self._updated
                self._updated = now
                self._tokens = min(self.burst, self._tokens + elapsed * self.rate)

                if self._tokens >= tokens:
                    self._tokens -= tokens
                    return waited

                wait = (tokens - self._tokens) / self.rate

            # 在锁外等待，避免阻塞其它线程
            time.sleep(wait)
            waited += wait
