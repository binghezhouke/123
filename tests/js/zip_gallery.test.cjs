const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function setup({count = 5, blobSize = 1, viewport = null, indexPages = null, delayIndex = false} = {}) {
    const documentHandlers = {};
    const element = () => ({dataset: {}, handlers: {}, children: [], isConnected: true,
        addEventListener(event, handler) {
            this.handlers[event] = handler;
            if (event === 'click') this.handlers[event] = supplied => {
                const value = supplied || {target: this, preventDefault() {}};
                handler(value); (documentHandlers.click || []).forEach(listener => listener(value));
            };
        },
        closest(selector) { return selector.includes('data-gallery-resume') && this.dataset.galleryResume !== undefined ? this : selector.includes('data-gallery-image') || selector.includes('.zip-image') ? this : null; },
        getAttribute() { return ''; },
        append(child) { this.children.push(child); },
        get childElementCount() { return this.children.length; },
        replaceChildren(...children) { this.children = children; },
        showModal() { this.open = true; },
        close() { this.open = false; this.handlers.close(); }});
    const elements = new Map();
    const get = id => {
        if (!elements.has(id)) elements.set(id, element());
        return elements.get(id);
    };
    const buttons = Array.from({length: count}, (_, i) => Object.assign(element(), {dataset: {url: `/member/${i}`, name: `${i}.jpg`, fileId: String(i)}}));
    const requests = [], indexRequests = [], revoked = [];
    const storage = new Map();
    let blobId = 0;
    vm.runInNewContext(fs.readFileSync('static/js/zip_gallery.js', 'utf8'), {
        document: {
            querySelectorAll: () => buttons, getElementById: get, createElement: () => element(),
            querySelector: selector => selector === '[data-gallery-resume]' ? get('gallery-resume') : null,
            addEventListener: (name, handler) => { (documentHandlers[name] ||= []).push(handler); },
            removeEventListener: name => { delete documentHandlers[name]; },
        },
        localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
        location: {pathname: '/directory/0', search: ''},
        AbortController, DOMException, Image: class {},
        createArchiveImageViewport: viewport ? () => viewport : undefined,
        URL: {createObjectURL: () => `blob:${++blobId}`, revokeObjectURL: url => revoked.push(url)},
        fetch: (url, {signal}) => {
            if (url === '/api/images') {
                if (!delayIndex) return Promise.resolve({ok: true, json: async () => ({items: indexPages, next: null})});
                return new Promise((resolve, reject) => { indexRequests.push({signal, resolve: () => resolve({ok:true,json:async()=>({items:indexPages,next:null})}), reject}); signal.addEventListener('abort',()=>reject(new DOMException('Aborted','AbortError'))); });
            }
            return new Promise((resolve, reject) => {
            requests.push({url, signal, resolve: () => resolve({ok: true, headers: {get: name => name === 'Server-Timing' ? 'extract;dur=12' : 'image/jpeg'}, blob: async () => ({size: typeof blobSize === 'function' ? blobSize(url) : blobSize})}), reject});
            signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
            });
        },
    });
    get('zip-gallery').dataset.galleryScope = 'directory:0';
    get('gallery-resume').dataset.galleryResume = '';
    if (indexPages) get('zip-gallery').dataset.galleryIndexUrl = '/api/images';
    for (const button of buttons) button.handlers.click = () => (documentHandlers.click || []).forEach(handler => handler({target:button, preventDefault() {}}));
    return {buttons, get, requests, revoked, storage, indexRequests, dispatchClick:target=>(documentHandlers.click||[]).forEach(handler=>handler({target,preventDefault(){}}))};
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

test('failed prefetch entries are attempted once and do not spin', async () => {
    const {buttons, get, requests} = setup({count: 5});
    buttons[0].handlers.click(); requests[0].done = true; requests[0].resolve(); await tick();
    get('zip-gallery-stage').children[0].onload();
    requests[1].done = true; requests[1].reject(new Error('network failure')); await tick();
    requests[2].done = true; requests[2].resolve(); await tick();
    await drainPrefetch(requests);
    assert.equal(requests.filter(request => request.url === '/member/1').length, 1);
});

test('gallery progress is stored by stable image id', async () => {
    const {buttons, get, requests, storage} = setup();
    buttons[3].handlers.click(); requests[0].resolve(); await tick();
    assert.deepEqual(JSON.parse(storage.get('galleryProgress:directory:0')), {identity: '3', name: '3.jpg'});
});

test('resume loads the metadata index before returning to a saved image', async () => {
    const {get, requests, storage, dispatchClick} = setup({count:3,indexPages:[
        {id:0,name:'0.jpg',url:'/member/0'}, {id:1,name:'1.jpg',url:'/member/1'}, {id:2,name:'2.jpg',url:'/member/2'},
    ]});
    storage.set('galleryProgress:directory:0',JSON.stringify({identity:'2',name:'2.jpg'}));
    dispatchClick(get('gallery-resume')); await tick(); await tick();
    assert.equal(requests[0].url,'/member/2');
    assert.equal(get('zip-gallery-counter').textContent,'3 / 3');
});

test('metadata index supports cross-page jump and reopening after the index is cached', async () => {
    const {buttons, get, requests} = setup({count: 3, indexPages: [
        {id: 0, name: '0.jpg', url: '/member/0'},
        {id: 1, name: '1.jpg', url: '/member/1'},
        {id: 2, name: '2.jpg', url: '/member/2'},
    ]});
    buttons[1].handlers.click(); await tick(); requests[0].resolve(); await tick();
    assert.equal(get('zip-gallery-counter').textContent, '2 / 3');
    get('zip-gallery-close').onclick();
    buttons[2].handlers.click(); await tick(); requests[1].resolve(); await tick();
    assert.equal(get('zip-gallery-counter').textContent, '3 / 3');
});

test('closing during a metadata fetch aborts it and a fresh open starts a new fetch', async () => {
    const {buttons, get, requests, indexRequests} = setup({count:3, delayIndex:true, indexPages:[
        {id:0,name:'0.jpg',url:'/member/0'}, {id:1,name:'1.jpg',url:'/member/1'}, {id:2,name:'2.jpg',url:'/member/2'},
    ]});
    buttons[0].handlers.click(); await tick(); assert.equal(indexRequests.length,1);
    get('zip-gallery-close').onclick(); assert.equal(indexRequests[0].signal.aborted,true);
    buttons[2].handlers.click(); await tick(); assert.equal(indexRequests.length,2);
    indexRequests[1].resolve(); await tick(); await tick();
    assert.equal(requests[0].url,'/member/2');
    assert.equal(get('zip-gallery-title').textContent,'2.jpg');
});

test('prefetch settings persist and cap background concurrency', async () => {
    const {buttons, get, requests, storage} = setup();
    get('zip-gallery-setting-count').value = '1';
    get('zip-gallery-setting-concurrency').value = '1';
    get('zip-gallery-setting-budget').value = '16';
    get('zip-gallery-settings-form').handlers.submit({preventDefault() {}});
    assert.deepEqual(JSON.parse(storage.get('imageGallerySettings')), {count: 1, concurrency: 1, budget: 16});
    buttons[0].handlers.click(); requests[0].resolve(); await tick(); get('zip-gallery-stage').children[0].onload();
    assert.equal(requests.length, 2);
});

test('diagnostics report request and server extraction time separately', async () => {
    const {buttons, get, requests} = setup();
    get('zip-gallery-diagnostics').open = true;
    buttons[0].handlers.click(); requests[0].resolve(); await tick();
    get('zip-gallery-stage').children[0].onload();
    assert.match(get('zip-gallery-diagnostics-output').textContent, /请求平均 .* ms/);
    assert.match(get('zip-gallery-diagnostics-output').textContent, /服务器解压平均 12 ms/);
    get('zip-gallery-diagnostics-reset').handlers.click({target:{closest:()=>null}});
    assert.match(get('zip-gallery-diagnostics-output').textContent, /服务器解压平均 未知/);
});

async function drainPrefetch(requests) {
    for (let round = 0; round < 12; round++) {
        const active = requests.filter(r => !r.done && !r.signal.aborted);
        if (!active.length) return;
        assert.ok(active.length <= 2);
        for (const request of active) { request.done = true; request.resolve(); }
        await tick();
    }
    assert.fail('prefetch must stop after filling the directional window');
}

test('reverse navigation prefetches nine previous images nearest first and rolls backward', async () => {
    const {buttons, get, requests} = setup({count: 20});
    await loadImage(buttons, get, requests, 19);
    get('zip-gallery').handlers.keydown({key: 'ArrowLeft', preventDefault() {}});
    requests.at(-1).done = true;
    requests.at(-1).resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    await drainPrefetch(requests);
    assert.deepEqual(requests.map(r => r.url), Array.from({length: 11}, (_, i) => `/member/${19 - i}`));
    get('zip-gallery-prev').onclick();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    assert.equal(requests.at(-1).url, '/member/8');
    assert.equal(requests.filter(r => r.url === '/member/17').length, 1);
});

test('reversing cancels obsolete forward requests and changing back cancels backward requests', async () => {
    const {buttons, get, requests} = setup({count: 30});
    await loadImage(buttons, get, requests, 10);
    const ahead = requests.filter(r => !r.done);
    assert.deepEqual(ahead.map(r => r.url), ['/member/11', '/member/12']);
    get('zip-gallery-prev').onclick();
    assert.ok(ahead.every(r => r.signal.aborted));
    requests.at(-1).done = true;
    requests.at(-1).resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    const behind = requests.filter(r => !r.done && !r.signal.aborted);
    assert.deepEqual(behind.map(r => r.url), ['/member/8', '/member/7']);
    get('zip-gallery-next').onclick();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    assert.ok(behind.every(r => r.signal.aborted));
    assert.deepEqual(requests.filter(r => !r.done && !r.signal.aborted).map(r => r.url), ['/member/11', '/member/12']);
});

test('retry preserves backward direction and a fresh open starts forward again', async () => {
    const {buttons, get, requests} = setup({count: 20});
    await loadImage(buttons, get, requests, 19);
    await loadImage(buttons, get, requests, 18);
    const behind = requests.filter(r => !r.done && !r.signal.aborted);
    assert.deepEqual(behind.map(r => r.url), ['/member/17', '/member/16']);
    get('zip-gallery-retry').onclick();
    assert.ok(behind.every(r => !r.signal.aborted));
    requests.at(-1).done = true;
    requests.at(-1).resolve();
    await tick();
    get('zip-gallery-stage').children[0].onload();
    assert.equal(requests.filter(r => r.url === '/member/19').length, 1);
    get('zip-gallery-close').onclick();
    await loadImage(buttons, get, requests, 10);
    assert.deepEqual(requests.filter(r => !r.done && !r.signal.aborted).map(r => r.url), ['/member/11', '/member/12']);
});


test('zoomed dragging and pinch do not flip images; fitting restores swipe and close releases viewport', async () => {
    let zoomed = true, attached = 0, detached = 0, closed = 0;
    const viewport = {isZoomed: () => zoomed, attach() {attached++;}, detach() {detached++;}, close() {closed++;}};
    const {buttons, get, requests} = setup({viewport});
    await loadImage(buttons, get, requests, 0);
    assert.equal(attached, 1);
    const stage = get('zip-gallery-stage');
    const swipe = () => {
        stage.handlers.touchstart({touches: [{clientX: 200}]});
        stage.handlers.touchend({changedTouches: [{clientX: 20}]});
    };
    swipe();
    assert.equal(get('zip-gallery-counter').textContent, '1 / 5');
    zoomed = false;
    stage.handlers.touchstart({touches: [{clientX: 200}, {clientX: 250}]});
    stage.handlers.touchend({changedTouches: [{clientX: 20}]});
    assert.equal(get('zip-gallery-counter').textContent, '1 / 5');
    swipe();
    assert.equal(get('zip-gallery-counter').textContent, '2 / 5');
    assert.equal(detached, 2);
    get('zip-gallery-close').onclick();
    assert.equal(closed, 1);
    await tick();
});
