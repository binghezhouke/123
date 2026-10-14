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
test('clicking an in-flight prefetch obtains foreground service before background work completes', async () => {
    const reader = new ImageResources(store()), background = new AbortController(), foreground = new AbortController();
    const requests = [];
    global.fetch = (_, opts) => {
        requests.push(opts);
        if (opts.priority === 'high') return Promise.resolve(response());
        return new Promise((resolve, reject) => {
            opts.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
        });
    };
    const prefetch = reader.read(item(), {signal: background.signal, priority: 'background'});
    prefetch.catch(() => {});
    await tick();
    assert.equal(requests[0].priority, 'low');
    const selected = reader.read(item(), {signal: foreground.signal});
    selected.catch(() => {});
    let timer;
    try {
        const result = await Promise.race([selected.then(() => 'displayed'), new Promise(resolve => timer = setTimeout(() => resolve('blocked behind background'), 100))]);
        assert.equal(result, 'displayed');
        assert.equal((await prefetch).blob, (await selected).blob, 'promotion preserves other consumers');
        assert.equal(requests.length, 2, 'replace the low-priority attempt only once');
        assert.equal(requests[0].signal.aborted, true);
        assert.equal(requests[1].priority, 'high');
        assert.deepEqual(requests[1].headers, {});
    } finally {
        clearTimeout(timer); background.abort(); foreground.abort();
    }
});
test('queued prefetch is promoted ahead of a full shared background pool without duplication', async () => {
    const reader = new ImageResources(store()), owner = new AbortController();
    const requests = [];
    let releaseForeground;
    global.fetch = (url, opts) => {
        requests.push({url, opts});
        return new Promise((resolve, reject) => {
            if (opts.priority === 'high') releaseForeground = () => resolve(response());
            opts.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
        });
    };
    const reads = Array.from({length: 9}, (_, i) => reader.read(item(`/file/${i}/preview`), {signal: owner.signal, priority: 'background'}));
    const all = Promise.allSettled(reads);
    await tick();
    assert.equal(requests.length, 4, 'all speculative callers share four slots');
    const next = item('/file/8/preview');
    const foreground = reader.read(next, {signal: owner.signal});
    await tick();
    assert.equal(requests.length, 5);
    assert.equal(requests[4].url, 'http://local.test/file/8/preview');
    assert.equal(requests[4].opts.priority, 'high');
    assert.equal(requests.filter(r => r.url.endsWith('/8/preview')).length, 1);
    releaseForeground(); await foreground;
    owner.abort(); await all;
    assert.equal(reader.backgroundActive, 0); assert.equal(reader.waiting.length, 0);
});
test('new speculation waits for selected bytes, and selection cancellation releases admission', async () => {
    const reader = new ImageResources(store()), selected = new AbortController(), bg = new AbortController();
    const requests = [];
    global.fetch = (url, opts) => {
        requests.push(url);
        return new Promise((resolve, reject) => opts.signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError'))));
    };
    const first = reader.read(item(), {signal: selected.signal});
    const firstCancelled = assert.rejects(first, {name: 'AbortError'});
    const second = reader.read(item('/file/9/preview'), {priority: 'background', signal: bg.signal});
    const secondCancelled = assert.rejects(second, {name: 'AbortError'});
    await tick(); assert.equal(requests.length, 1);
    selected.abort(); await firstCancelled; await tick();
    assert.equal(requests.length, 2);
    bg.abort(); await secondCancelled;
});
test('completed image displays before persistent storage finishes and remains reusable', async () => {
    const cache = new OriginalStore(), reader = new ImageResources(cache);
    let unlock, calls = 0;
    const put = cache.put.bind(cache);
    cache.put = (key, version, blob) => {
        cache.db = new Promise(resolve => unlock = resolve);
        return put(key, version, blob);
    };
    global.fetch = async () => { calls++; return response(); };
    const displayed = await reader.read(item());
    assert.equal(displayed.blob.size, 16);
    assert.equal((await reader.read(item())).cached, true);
    assert.equal(calls, 1);
    unlock(null);
});
test('CDN bodies do not consume application-origin background slots after redirect', async () => {
    const reader = new ImageResources(store()), owner = new AbortController();
    let calls = 0;
    global.fetch = async (_, {signal}) => {
        calls++;
        return {ok: true, url: 'https://cdn.example/image.png', headers: new Headers({'Content-Type': 'image/png'}),
            blob: () => new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError'))))};
    };
    const reads = Array.from({length: 9}, (_, i) => {
        const node = item(`/file/${i}/preview`);
        node.dataset.directUrl = `/file/${i}/preview?direct=1`;
        return reader.read(node, {priority: 'background', signal: owner.signal});
    });
    const all = Promise.allSettled(reads);
    await tick();
    assert.equal(calls, 9); assert.equal(reader.backgroundActive, 0);
    owner.abort(); await all;
});
test('gallery promotion replaces a stalled response body while keeping the shared result', async () => {
    const reader = new ImageResources(store());
    let calls = 0;
    global.fetch = async (_, opts) => {
        calls++;
        if (opts.priority === 'high') return response();
        return new Response(new ReadableStream({start(controller) {
            controller.enqueue(new Uint8Array([1, 2, 3]));
            opts.signal.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')));
        }}), {headers: {'Content-Type': 'image/png'}});
    };
    const pending = reader.read(item(), {priority: 'background'});
    await tick();
    reader.promote(item());
    assert.equal((await pending).blob.size, 16);
    assert.equal(calls, 2);
    assert.equal(reader.pending.size, 0);
    assert.equal(reader.foreground.size, 0);
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
