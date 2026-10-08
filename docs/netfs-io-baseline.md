# Local I/O statistics baseline

This baseline is reproducible without a live mount or account credentials. It uses an in-process HTTP server that serves immutable, correctly ranged bytes and sleeps 2 ms before each response body. The test exercises cold and repeated reads, four different files read concurrently, and a consumer that pauses 3 ms between 64 KiB reads. It logs scenario elapsed time, application bytes, HTTP request/response bytes, effective read rate, and the aggregate mount/storage snapshot. The values are measured on the test host each run; they are not portable performance claims.

Run from the `mount123` module:

```sh
GOTOOLCHAIN=local GOPROXY=off /tmp/123-go-sdk/go/bin/go test -race -buildvcs=false -run '^TestIOStatsLocalRangeColdHotMultiFileAndSlowConsumer$' -v ./internal/storage
```

The first remote's one-byte validator probe occurs before the cold scenario timer; multi-file and slow-consumer scenarios include each reader's validator probe in their timings. The hot scenario verifies that repeating the same range issues no HTTP request. The store uses a fresh durable cache per test run. The test logs one JSON line beginning `local Range baseline:`; retain that line with the host, Go version, and revision when comparing changes.

One recorded run on 2026-10-08 used Go 1.27.1 on Linux `7.0.14-19-pve` x86_64 with the test's fixed 2 ms response delay. Cold read: 5.83 ms, 1 request and 1 MiB returned for 256 KiB consumed (44.9 MB/s effective). Hot repeat: 0 requests, 0 network bytes, 512 MB/s effective over the short 0.51 ms call. Four-file mixed run: 11.60 ms, 8 requests and 4 MiB plus four 1-byte probes (45.2 MB/s effective). Slow consumer: 33.32 ms, four 1 MiB responses for four 64 KiB reads at 1 MiB offsets (7.87 MB/s effective). Aggregate tracker values for this run were 9,437,190 downloaded bytes, 262,144 cache-hit bytes, 15 HTTP body timing samples, 15 transfer-gate wait samples, and 9 cache publication samples. These figures describe this localhost harness only; the deliberately sparse slow-consumer case demonstrates amplification rather than expected user throughput. The detailed JSON is emitted by the test so future revisions can be compared under the same host and harness.

The test also asserts the statistics distinguish unknown from measured zero, record bytes received even for an incomplete response body, include cache-hit bytes on the repeated read, and publish cache latency and HTTP body timing. The transfer timing starts at the first body byte and ends when the response body closes; body TTFB starts immediately before `http.Client.Do`, after waiting for the local transfer gate. It therefore excludes local queue time, which is reported separately. The metric is first body byte, not merely response headers.

`mount123 io-stats -control-socket PATH` returns a machine-readable JSON snapshot from the running process. Only process-wide aggregates are exposed; no file names, URLs, credentials, passwords, or file data are included. A metric with `status: "unknown"` has JSON `null` numeric fields; this means either no hook is implemented yet or no observations have occurred since startup. `status: "measured"` with zero bytes means the hook ran and observed zero bytes. Directory lookup statistics remain unknown because they are not instrumented yet.

This local test bypasses kernel FUSE page-cache behavior and remote CDN variability. Use it to compare storage-side request and cache behavior. For a mounted end-to-end run, keep the same remote file set and access pattern, record `io-stats` before and after each scenario, and report the difference alongside FUSE application elapsed time. Separate cold cache, same-process hot cache, and remount cache results; do not label a remount reuse run as cold.
