const test = require('node:test');
const assert = require('node:assert/strict');
const {compare, intervalMean, History, bytes, percent} = require('../static/js/status.js');
const sample = (t = 1) => ({schema_version: 1, runtime: {}, web: {completed: 0, response_bytes: 0}, io: {
    started_at: '2026-01-01T00:00:00Z', collected_at: new Date(1767225600000 + t * 1000).toISOString(), uptime_seconds: t,
    downloaded_bytes: {status: 'measured', bytes: 0}, cache: {foreground: {status: 'measured', hit_bytes: 0, requested_bytes: 0}},
    download_scheduler: {status: 'measured', active_requests: 1, waiting_foreground: 2, waiting_background: 3}, index_queue: {status: 'measured', waiting: 0}
}});
test('rates use interval bytes, not lifetime or cumulative percentiles', () => {
    const a = sample(5), b = sample(10);
    a.io.downloaded_bytes.bytes = 1000; b.io.downloaded_bytes.bytes = 6000;
    a.web.response_bytes = 2000; b.web.response_bytes = 12000;
    b.io.cache.foreground.requested_bytes = 100; b.io.cache.foreground.hit_bytes = 75;
    assert.deepEqual(compare(a, b), {seconds: 5, download_rate: 1000, web_rate: 2000, hit_ratio: .75});
});
test('idle cache interval is unknown; first hooked download event starts at zero', () => {
    const a = sample(), b = sample(6);
    a.io.downloaded_bytes = {status: 'unknown', bytes: null}; b.io.downloaded_bytes.bytes = 50;
    assert.equal(compare(a, b).download_rate, 10); assert.equal(compare(a, b).hit_ratio, null);
    delete a.io.downloaded_bytes; assert.equal(compare(a, b).download_rate, null);
});
test('restarts, reset counters and duplicate timestamps never generate rates', () => {
    const a = sample(), b = sample(6);
    assert.equal(compare(a, a), null);
    b.io.started_at = 'different'; assert.equal(compare(a, b), null);
    b.io.started_at = a.io.started_at; a.web.response_bytes = 10;
    assert.equal(compare(a, b), null);
    a.web.response_bytes = 0; a.web.completed = 1; assert.equal(compare(a, b), null);
});
test('interval latency only uses new observations; unknown is not 0 ms', () => {
    const a = {samples: 2, total_nanos: 100, p95_nanos: 64};
    assert.equal(intervalMean(a, {samples: 3, total_nanos: 180, p95_nanos: 128}), 80);
    assert.equal(intervalMean(a, a), null);
    assert.equal(intervalMean(a, {samples: 1, total_nanos: 40}), null);
    assert.equal(intervalMean({status: 'unknown', samples: null, total_nanos: null}, a), 50);
});
test('history bounds memory, ignores duplicates and starts new epochs', () => {
    const h = new History(3);
    for (let i = 1; i <= 5; i++) h.add(sample(i));
    assert.equal(h.points.length, 3); assert.equal(h.add(sample(5)), null);
    h.gap(); assert.equal(h.add(sample(6)).interval, null);
    assert.notEqual(h.add(sample(7)).interval, null);
    const next = sample(8); next.io.started_at = 'new-process'; h.add(next);
    assert.equal(h.points.length, 1);
    assert.equal(h.points[0].download_rate, undefined);
});
test('zero and unavailable metrics stay distinct', () => {
    assert.equal(bytes(null), '—'); assert.equal(bytes(0), '0 B');
    assert.equal(percent(null), '—'); assert.equal(percent(0), '0.0%');
});
