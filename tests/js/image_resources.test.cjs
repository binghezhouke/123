const {test} = require('node:test');
const assert = require('node:assert/strict');
global.location = {href: 'http://local.test/browse'};
const {ImageResources, OriginalStore, identity} = require('../../mount123/internal/webserve/static/js/image_resources.js');
const tick = () => new Promise(resolve => setImmediate(resolve));
const item = (url = '/archive/1/member?path=one.png') => ({dataset: {url}});
const response = () => new Response(new Blob(['encoded-original'], {type: 'image/png'}), {headers: {'Content-Type': 'image/png'}});
function store() {
    const rows = new Map();
    return {rows,
        async get(key, version) { const row = rows.get(key); return row && (!version || !row.version || row.version === version) ? row.blob : null; },
        async put(key, version, blob) { rows.set(key, {version, blob}); },
        async remove(key) { rows.delete(key); }};
}
test('thumbnail, preview and gallery share one transfer; cancellation releases only its owner', async () => {
    const cache = store(), reader = new ImageResources(cache), thumb = new AbortController();
    let requests = 0, complete, sourceSignal;
    global.fetch = (_, {signal}) => { requests++; sourceSignal = signal; return new Promise(resolve => complete = () => resolve(response())); };
    const a = reader.read(item(), {signal: thumb.signal, priority: 'background'});
    const rejected = assert.rejects(a, {name: 'AbortError'});
    const b = reader.read(item()), c = reader.read(item());
    await tick(); assert.equal(requests, 1);
    thumb.abort(); await rejected;
    assert.equal(sourceSignal.aborted, false);
    complete();
    const [one, two] = await Promise.all([b, c]);
    assert.equal(one.blob, two.blob);
    assert.equal(reader.pending.size, 0);
    assert.equal((await reader.read(item())).cached, true);
    assert.equal(requests, 1);
});
test('last cancellation aborts abandoned transfer and a fresh request can retry', async () => {
    const reader = new ImageResources(store()), controller = new AbortController();
    let sourceSignal;
    global.fetch = (_, {signal}) => new Promise((resolve, reject) => {
        sourceSignal = signal; signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
    });
    const waiting = reader.read(item(), {signal: controller.signal});
    const rejected = assert.rejects(waiting, {name: 'AbortError'});
    await tick(); controller.abort(); await rejected;
    assert.equal(sourceSignal.aborted, true); assert.equal(reader.pending.size, 0);
    global.fetch = async () => response();
    assert.equal((await reader.read(item())).blob.size, 16);
});
test('HTTP errors do not poison encoded cache; explicit forget forces retry', async () => {
    const cache = store(), reader = new ImageResources(cache);
    global.fetch = async () => new Response('denied', {status: 403});
    await assert.rejects(reader.read(item()), /403/); assert.equal(cache.rows.size, 0);
    let calls = 0; global.fetch = async () => { calls++; return response(); };
    await reader.read(item()); await reader.read(item()); assert.equal(calls, 1);
    await reader.forget(item()); await reader.read(item()); assert.equal(calls, 2);
});
test('direct fallback has no custom CORS header and foreground reuses its stable local identity', async () => {
    const reader = new ImageResources(store()), node = item('/file/7/preview');
    node.dataset.directUrl = '/file/7/preview?direct=1';
    const requests = [];
    global.fetch = async (url, opts) => {
        requests.push({url, opts});
        if (url.includes('direct=1')) throw new TypeError('CORS');
        return response();
    };
    await reader.read(node, {priority: 'background'});
    await reader.read(node);
    assert.equal(requests.length, 2);
    assert.deepEqual(requests[0].opts.headers, {});
    assert.equal(requests[1].opts.headers['X-Priority'], 'background');
    assert.equal(identity(item('/file/7/preview?direct=1')), identity(node));
});
test('member identities stay isolated; plain file metadata changes invalidate originals', async () => {
    const cache = store(), reader = new ImageResources(cache); let calls = 0;
    global.fetch = async () => { calls++; return response(); };
    await reader.read(item('/archive/1/member?path=one.png'));
    await reader.read(item('/archive/2/member?path=one.png'));
    assert.equal(calls, 2);
    const node = item('/file/7/preview'); node.dataset.updated = 'old';
    await reader.read(node); await reader.read(node); assert.equal(calls, 3);
    node.dataset.updated = 'new'; await reader.read(node); assert.equal(calls, 4);
});
test('storage-unavailable fallback retains at most four encoded originals', async () => {
    const cache = new OriginalStore();
    for (let i = 0; i < 7; i++) await cache.put(String(i), '', new Blob([String(i)]));
    assert.equal(cache.fallback.size, 4);
    assert.equal(await cache.get('0', ''), null);
    assert.equal(await (await cache.get('6', '')).text(), '6');
});
