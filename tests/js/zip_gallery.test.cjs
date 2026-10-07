const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function setup({count = 5, blobSize = 1} = {}) {
    const element = () => ({dataset: {}, handlers: {}, children: [],
        addEventListener(event, handler) { this.handlers[event] = handler; },
        replaceChildren(...children) { this.children = children; },
        showModal() { this.open = true; },
        close() { this.open = false; this.handlers.close(); }});
    const elements = new Map();
    const get = id => {
        if (!elements.has(id)) elements.set(id, element());
        return elements.get(id);
    };
    const buttons = Array.from({length: count}, (_, i) => Object.assign(element(), {dataset: {url: `/member/${i}`, name: `${i}.jpg`}}));
    const requests = [], revoked = [];
    let blobId = 0;
    vm.runInNewContext(fs.readFileSync('static/js/zip_gallery.js', 'utf8'), {
        document: {querySelectorAll: () => buttons, getElementById: get},
        AbortController, DOMException, Image: class {},
        URL: {createObjectURL: () => `blob:${++blobId}`, revokeObjectURL: url => revoked.push(url)},
        fetch: (url, {signal}) => new Promise((resolve, reject) => {
            requests.push({url, signal, resolve: () => resolve({ok: true, headers: {get: () => 'image/jpeg'}, blob: async () => ({size: typeof blobSize === 'function' ? blobSize(url) : blobSize})}), reject});
            signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
        }),
    });
    return {buttons, get, requests, revoked};
}
const tick = () => new Promise(resolve => setImmediate(resolve));

test('switching to an in-flight prefetched image reuses the request', async () => {
    const {buttons, get, requests} = setup();
    buttons[0].handlers.click();
    requests[0].resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    assert.equal(requests.length, 3);
    get('zip-gallery-next').onclick();
    assert.equal(requests.length, 3);
    assert.equal(requests[1].signal.aborted, false);
    requests[1].resolve();
    await tick();
    assert.equal(get('zip-gallery-stage').children[0].alt, '1.jpg');
    get('zip-gallery-prev').onclick();
    await tick();
    assert.equal(requests.length, 3);
    assert.equal(get('zip-gallery-stage').children[0].alt, '0.jpg');
});

test('closing cancels prefetch and releases cached blobs', async () => {
    const {buttons, get, requests, revoked} = setup();
    buttons[0].handlers.click();
    requests[0].resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    get('zip-gallery-close').onclick();
    await tick();
    assert.equal(requests[1].signal.aborted, true);
    assert.deepEqual(revoked, ['blob:1']);
    assert.equal(get('zip-gallery-stage').children.length, 0);
});

test('jumping away cancels irrelevant requests and ignores stale completions', async () => {
    const {buttons, get, requests} = setup();
    buttons[0].handlers.click();
    get('zip-gallery-next').onclick();
    get('zip-gallery-next').onclick();
    assert.equal(requests[0].signal.aborted, true);
    requests[2].resolve();
    await tick();
    requests[1].resolve();
    await tick();
    assert.equal(get('zip-gallery-stage').children[0].alt, '2.jpg');
});

async function loadImage(buttons, get, requests, index) {
    buttons[index].handlers.click();
    await tick();
    const request = requests.find(item => item.url === `/member/${index}` && !item.done);
    if (request) {
        request.done = true;
        request.resolve();
        await tick();
    }
    const image = get('zip-gallery-stage').children[0];
    if (image?.onload) image.onload();
    await tick();
}

test('returns to an image after navigating beyond the old three-image limit without refetching', async () => {
    const {buttons, get, requests} = setup({count: 7});
    for (let i = 0; i < 6; i++) await loadImage(buttons, get, requests, i);
    const before = requests.filter(item => item.url === '/member/0').length;
    await loadImage(buttons, get, requests, 0);
    assert.equal(requests.filter(item => item.url === '/member/0').length, before);
    assert.equal(get('zip-gallery-stage').children[0].alt, '0.jpg');
});

test('evicts least recently used images after 30 cached images', async () => {
    const {buttons, get, requests} = setup({count: 33});
    for (let i = 0; i <= 30; i++) await loadImage(buttons, get, requests, i);
    const before = requests.filter(item => item.url === '/member/0').length;
    await loadImage(buttons, get, requests, 0);
    assert.equal(requests.filter(item => item.url === '/member/0').length, before + 1);
    assert.equal(get('zip-gallery-stage').children[0].alt, '0.jpg');
});

test('evicts by blob byte size and retains the selected image', async () => {
    const {buttons, get, requests} = setup({count: 12, blobSize: 30 * 1024 * 1024});
    for (let i = 0; i < 10; i++) await loadImage(buttons, get, requests, i);
    const before = requests.filter(item => item.url === '/member/9').length;
    await loadImage(buttons, get, requests, 9);
    assert.equal(requests.filter(item => item.url === '/member/9').length, before);
    assert.equal(get('zip-gallery-stage').children[0].alt, '9.jpg');
    await loadImage(buttons, get, requests, 0);
    assert.equal(requests.filter(item => item.url === '/member/0').length, 2);
});

test('prefetches nine ahead with two concurrent downloads, then rolls forward', async () => {
    const {buttons, get, requests} = setup({count: 20});
    buttons[0].handlers.click();
    requests[0].done = true;
    requests[0].resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    while (requests.some(r => !r.done && !r.signal.aborted)) {
        const active = requests.filter(r => !r.done && !r.signal.aborted);
        assert.ok(active.length <= 2);
        for (const request of active) {
            request.done = true;
            request.resolve();
        }
        await tick();
    }
    assert.deepEqual(requests.map(r => r.url), Array.from({length: 10}, (_, i) => `/member/${i}`));
    get('zip-gallery-next').onclick();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    assert.equal(requests.at(-1).url, '/member/10');
    assert.equal(requests.filter(r => r.url === '/member/1').length, 1);
    get('zip-gallery-close').onclick();
    await tick();
    assert.equal(requests.at(-1).signal.aborted, true);
});

test('byte-budget eviction does not cause endless prefetch refetching', async () => {
    const {buttons, get, requests} = setup({count: 20, blobSize: 32 * 1024 * 1024});
    buttons[0].handlers.click();
    requests[0].done = true;
    requests[0].resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    for (let rounds = 0; rounds < 12; rounds++) {
        const active = requests.filter(r => !r.done && !r.signal.aborted);
        if (!active.length) break;
        for (const request of active) { request.done = true; request.resolve(); }
        await tick();
    }
    assert.equal(requests.length, 10);
    assert.equal(requests.filter(r => !r.done).length, 0);
    assert.equal(get('zip-gallery-stage').children[0].alt, '0.jpg');
});
