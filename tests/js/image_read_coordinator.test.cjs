const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function load(fetch) {
    const context = {fetch, AbortController, DOMException, document: {addEventListener() {}}, window: {addEventListener() {}}};
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/image_read_coordinator.js', 'utf8'), context);
    return context.imageReadCoordinator;
}
const response = () => ({ok: true, status: 200, headers: {}, blob: async () => ({size: 1})});
test('shares an in-flight response body and gives each consumer a response view', async () => {
    let calls = 0;
    const coordinator = load(async () => { calls++; await new Promise(resolve => setTimeout(resolve, 5)); return response(); });
    const [one, two] = await Promise.all([coordinator.fetch('/image'), coordinator.fetch('/image')]);
    assert.equal(calls, 1); assert.equal(one.ok, true); assert.equal(two.ok, true); assert.equal(coordinator.stats.shared, 1);
});
test('aborting one consumer leaves a shared body read available to another', async () => {
    let resolve;
    const coordinator = load(() => new Promise(r => { resolve = r; }));
    const first = new AbortController(), second = new AbortController();
    const one = coordinator.fetch('/image', {signal: first.signal}).catch(error => error.name);
    const two = coordinator.fetch('/image', {signal: second.signal});
    first.abort(); resolve(response());
    assert.equal(await one, 'AbortError'); assert.equal((await two).ok, true);
});
test('foreground and background scheduling hints share one read', async () => {
    let calls = 0;
    const coordinator = load(async () => { calls++; return response(); });
    await Promise.all([
        coordinator.fetch('/image', {headers: {'X-Priority': 'background'}, priority: 'low'}),
        coordinator.fetch('/image', {priority: 'high'}),
    ]);
    assert.equal(calls, 1);
});
test('queued foreground work starts before queued background work', async () => {
    const calls = [];
    const coordinator = load((url, options) => new Promise(resolve => calls.push({url, options, resolve})));
    const background = Array.from({length: 11}, (_, i) => coordinator.fetch(`/background-${i}`, {priority: 'low'}).catch(() => null));
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(coordinator.active, 9); assert.equal(coordinator.queued, 2);
    const foreground = coordinator.fetch('/foreground', {priority: 'high'}).catch(() => null);
    await new Promise(resolve => setImmediate(resolve));
    calls[0].resolve(response());
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(calls[9].url, '/foreground');
    coordinator.clear(); await Promise.all([...background, foreground]);
});
test('clearing the coordinator aborts every active transport', async () => {
    const signals = [];
    const coordinator = load((url, options) => { signals.push(options.signal); return new Promise(() => {}); });
    const reads = [coordinator.fetch('/one').catch(() => null), coordinator.fetch('/two').catch(() => null)];
    await new Promise(resolve => setImmediate(resolve));
    coordinator.clear(); await Promise.all(reads);
    assert.equal(signals.length, 2); assert.ok(signals.every(signal => signal.aborted));
});
