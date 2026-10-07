const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function setup() {
    const el = () => ({dataset: {}, handlers: {}, hidden: false, attributes: {},
        addEventListener(type, fn) { this.handlers[type] = fn; },
        removeAttribute(name) { delete this.attributes[name]; },
        setAttribute(name, value) { this.attributes[name] = value; },
        pause() {}, load() {}, showModal() { this.open = true; }, close() { this.open = false; this.handlers.close?.(); }});
    const elements = new Map();
    const get = id => { if (!elements.has(id)) elements.set(id, el()); return elements.get(id); };
    const buttons = [0, 1].map(i => Object.assign(el(), {dataset: {url: `/video/${i}`, name: `v${i}.mp4`}}));
    const workers = [], fetches = [], revoked = [], created = [];
    let nextBlob = 0;
    class WorkerMock {
        constructor(url) { this.url = url; this.terminated = false; workers.push(this); }
        postMessage(message) { this.start = message; }
        terminate() { this.terminated = true; }
        send(data) { this.onmessage({data}); }
    }
    const fetch = (url, options) => new Promise((resolve, reject) => {
        const request = {url, options, resolve: () => resolve({ok: true, json: async () => ({key: `f:v:${url}`, raw_url: `/raw${url}`, size: 20, compressed_size: 10, crc32: 123, mimetype: 'video/mp4', filename: 'video.mp4'})}), reject};
        fetches.push(request);
        options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
    });
    vm.runInNewContext(fs.readFileSync('static/js/zip_video_client.js', 'utf8'), {
        document: {querySelectorAll: () => buttons, getElementById: get},
        AbortController, DOMException,
        window: {addEventListener() {}}, Worker: WorkerMock, fetch, Blob,
        URL: {createObjectURL: () => { const url = `blob:${++nextBlob}`; created.push(url); return url; }, revokeObjectURL: url => revoked.push(url)},
    });
    return {buttons, get, workers, fetches, revoked, created};
}
const tick = () => new Promise(resolve => setImmediate(resolve));
async function finishSetup(request, workers) { request.resolve(); await tick(); return workers.at(-1); }
const validBlob = () => new Blob(['12345678901234567890']);

test('closing cancels fetch or worker and keeps a completed video for reopening', async () => {
    const {buttons, get, fetches, workers, revoked} = setup();
    buttons[0].handlers.click();
    get('zip-client-video-close').handlers.click();
    assert.equal(fetches[0].options.signal.aborted, true);
    await tick();
    buttons[0].handlers.click();
    const worker = await finishSetup(fetches[1], workers);
    worker.send({type: 'done', blob: validBlob()});
    buttons[0].handlers.click();
    assert.equal(fetches.length, 2);
    assert.equal(get('zip-client-video-download').href, 'blob:1');
    get('zip-client-video-close').handlers.click();
    assert.deepEqual(revoked, []);
});

test('switching video revokes cached object URL and terminates the previous worker', async () => {
    const {buttons, get, fetches, workers, revoked} = setup();
    buttons[0].handlers.click();
    const worker = await finishSetup(fetches[0], workers);
    worker.send({type: 'done', blob: validBlob()});
    buttons[1].handlers.click();
    assert.deepEqual(revoked, ['blob:1']);
    assert.equal(fetches.length, 2);
});

test('cancelling an active worker terminates it and ignores a late result', async () => {
    const {buttons, get, fetches, workers} = setup();
    buttons[0].handlers.click();
    const worker = await finishSetup(fetches[0], workers);
    get('zip-client-video-cancel').handlers.click();
    assert.equal(worker.terminated, true);
    worker.send({type: 'done', blob: new Blob(['late'])});
    assert.equal(get('zip-client-video-download').hidden, true);
    assert.equal(get('zip-client-video-status').textContent, '已取消。');
});

test('failure can retry and late messages from the old generation are ignored', async () => {
    const {buttons, get, fetches, workers} = setup();
    buttons[0].handlers.click();
    const oldWorker = await finishSetup(fetches[0], workers);
    oldWorker.send({type: 'error', message: 'bad crc'});
    assert.equal(get('zip-client-video-status').textContent, 'bad crc');
    get('zip-client-video-retry').handlers.click();
    const retryWorker = await finishSetup(fetches[1], workers);
    oldWorker.send({type: 'done', blob: new Blob(['stale'])});
    assert.equal(get('zip-client-video-download').hidden, true);
    retryWorker.send({type: 'done', blob: validBlob()});
    assert.equal(get('zip-client-video-download').hidden, false);
});

test('repeated click during preparation does not start another request', async () => {
    const {buttons, fetches} = setup();
    buttons[0].handlers.click();
    buttons[0].handlers.click();
    assert.equal(fetches.length, 1);
});

test('progress uses uncompressed size and cached playback uses a completed range', async () => {
    const {buttons, get, fetches, workers} = setup();
    buttons[0].handlers.click();
    const worker = await finishSetup(fetches[0], workers);
    worker.send({type: 'progress', received: 4, produced: 10, total: 4, size: 20});
    assert.equal(get('zip-client-video-progress').max, 20);
    assert.equal(get('zip-client-video-progress').value, 10);
    worker.send({type: 'done', blob: validBlob()});
    assert.equal(get('zip-client-video-progress').max, 1);
    assert.equal(get('zip-client-video-progress').value, 1);
});

test('invalid worker blob is rejected before an object URL is cached', async () => {
    const {buttons, get, fetches, workers, created} = setup();
    buttons[0].handlers.click();
    const worker = await finishSetup(fetches[0], workers);
    worker.send({type: 'done', blob: new Blob(['short'])});
    assert.deepEqual(created, []);
    assert.equal(get('zip-client-video-retry').hidden, false);
    assert.equal(get('zip-client-video-download').hidden, true);
});

test('playback error keeps the validated download and retry releases it before fetching again', async () => {
    const {buttons, get, fetches, workers, revoked} = setup();
    buttons[0].handlers.click();
    const worker = await finishSetup(fetches[0], workers);
    worker.send({type: 'done', blob: validBlob()});
    get('zip-client-video-player').handlers.error();
    assert.equal(get('zip-client-video-status').textContent, '准备已完成，但浏览器不支持此视频编码或视频损坏');
    assert.equal(get('zip-client-video-download').hidden, false);
    get('zip-client-video-retry').handlers.click();
    assert.deepEqual(revoked, ['blob:1']);
    assert.equal(fetches.length, 2);
});
