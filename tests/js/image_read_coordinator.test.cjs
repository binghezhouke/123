const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function load(fetch) {
    const context = {fetch, AbortController, DOMException, document: {addEventListener() {}}, window: {addEventListener() {}}};
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/image_read_coordinator.js', 'utf8'), context);
    return context.imageReadCoordinator;
}
test('shares an in-flight response and clones it for each consumer', async () => {
    let calls = 0;
    const coordinator = load(async () => { calls++; await new Promise(resolve => setTimeout(resolve, 5)); return {clone: () => ({ok: true})}; });
    const [one, two] = await Promise.all([coordinator.fetch('/image'), coordinator.fetch('/image')]);
    assert.equal(calls, 1); assert.deepEqual(one, {ok: true}); assert.deepEqual(two, {ok: true}); assert.equal(coordinator.stats.shared, 1);
});
test('aborting one consumer leaves a shared read available to another', async () => {
    let resolve;
    const coordinator = load(() => new Promise(r => { resolve = r; }));
    const first = new AbortController(), second = new AbortController();
    const one = coordinator.fetch('/image', {signal: first.signal}).catch(error => error.name);
    const two = coordinator.fetch('/image', {signal: second.signal});
    first.abort(); resolve({clone: () => ({ok: true})});
    assert.equal(await one, 'AbortError'); assert.deepEqual(await two, {ok: true});
});
