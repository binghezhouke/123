const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');

function setup({count = 5, blobSize = 1, viewport = null, indexPages = null, delayIndex = false, pages = [], delayedPage = false, manualTimers = false, delayedExif = false} = {}) {
    const documentHandlers = {};
    const element = () => ({dataset: {}, attrs: {}, handlers: {}, listeners: {}, children: [], isConnected: true,
        addEventListener(event, handler) {
            (this.listeners[event] ||= []).push(handler);
            this.handlers[event] = handler;
            if (event === 'click') this.handlers[event] = supplied => {
                const value = supplied || {target: this, preventDefault() {}};
                handler(value); (documentHandlers.click || []).forEach(listener => listener(value));
            };
        },
        removeEventListener(event, handler) {this.listeners[event] = (this.listeners[event] || []).filter(fn => fn !== handler);},
        closest(selector) { return selector.includes('data-gallery-resume') && this.dataset.galleryResume !== undefined ? this : selector.includes('data-gallery-image') || selector.includes('.zip-image') ? this : null; },
        getAttribute(name) { return this.attrs[name] ?? null; },
        setAttribute(name, value) {this.attrs[name] = String(value);},
        removeAttribute(name) {delete this.attrs[name];},
        get max() {return this.attrs.max || '';},
        set max(value) {this.setAttribute('max', value);},
        checkValidity() {return Number.isInteger(Number(this.value)) && Number(this.value) >= 1 && (!this.max || Number(this.value) <= Number(this.max));},
        append(child) { this.children.push(child); },
        get childElementCount() { return this.children.length; },
        replaceChildren(...children) { this.children = children; },
        showModal() { this.open = true; },
        close() { this.open = false; (this.listeners.close || []).slice().forEach(fn => fn()); }});
    const elements = new Map();
    const get = id => {
        if (!elements.has(id)) elements.set(id, element());
        return elements.get(id);
    };
    const buttons = Array.from({length: count}, (_, i) => Object.assign(element(), {dataset: {url: `/member/${i}`, name: `${i}.jpg`, fileId: String(i)}}));
    const exif = [];
    const timers = new Map(); let timerID = 0;
    const paging = {calls: 0, remaining: [...pages], pending: []};
    const window = {
        hasNextFilePage: () => paging.remaining.length > 0,
        loadNextFilePage: () => {
            paging.calls++;
            const page = paging.remaining.shift();
            const append = () => {
                for (let i = 0; i < page; i++) {
                    const index = buttons.length;
                    buttons.push(Object.assign(element(), {dataset: {url: `/member/${index}`, name: `${index}.jpg`, fileId: String(index)}}));
                }
                return true;
            };
            return delayedPage ? new Promise(resolve => paging.pending.push(() => resolve(append()))) : Promise.resolve(append());
        },
    };
    const requests = [], indexRequests = [], revoked = [], images = [];
    const storage = new Map();
    let blobId = 0;
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/zip_gallery.js', 'utf8'), {
        window,
        setTimeout: manualTimers ? (fn, delay) => {const id = ++timerID; timers.set(id, {fn, delay}); return id;} : setTimeout,
        clearTimeout: manualTimers ? id => timers.delete(id) : clearTimeout,
        document: {
            querySelectorAll: () => buttons, getElementById: get, createElement: () => element(),
            querySelector: selector => selector === '[data-gallery-resume]' ? get('gallery-resume') : null,
            addEventListener: (name, handler) => { (documentHandlers[name] ||= []).push(handler); },
            removeEventListener: (name, handler) => {documentHandlers[name] = (documentHandlers[name] || []).filter(fn => fn !== handler);},
        },
        localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
        location: {href: 'http://localhost/browse', pathname: '/directory/0', search: ''},
        AbortController, DOMException, Image: class { constructor() { images.push(this); this.naturalWidth=100; this.naturalHeight=100; } set src(value) { this.assignments=(this.assignments||0)+1; this.url=value; } },
        readImageExif: delayedExif ? () => new Promise(resolve => exif.push(resolve)) : undefined,
        createArchiveImageViewport: viewport ? () => viewport : undefined,
        URL: Object.assign(URL, {createObjectURL: () => `blob:${++blobId}`, revokeObjectURL: url => revoked.push(url)}),
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
    return {exif, timers, paging, images, buttons, get, requests, revoked, storage, indexRequests, dispatchClick:target=>(documentHandlers.click||[]).forEach(handler=>handler({target,preventDefault(){}}))};
}
const tick = () => new Promise(resolve => setImmediate(resolve));


test('Go gallery keeps encoded blobs and only creates an Image for the selected slide', async () => {
    const {buttons,get,images,requests,revoked}=setup({count:3});
    buttons.forEach((b,i)=>b.dataset.url='/file/'+i+'/preview?direct=1');
    buttons[0].handlers.click();
    assert.equal(images.length,0);
    requests[0].resolve(); await tick();
    assert.equal(images.length,1);
    const nextRequest=requests.find(r=>r.url.includes('/file/1/'));
    assert.ok(nextRequest);
    nextRequest.resolve(); await tick();
    assert.equal(images.length,1, 'prefetch must not create decoded images');
    const count=requests.length;
    get('zip-gallery-next').onclick(); await tick();
    assert.equal(images.length,2);
    get('zip-gallery-prev').onclick(); await tick();
    assert.equal(requests.length,count, 'back navigation reuses encoded blob');
    get('zip-gallery-close').onclick(); await tick();
    assert.ok(revoked.length >= 2);
});


test('Go archive gallery advances past the 60-row shell without a load-more link', async () => {
    const {buttons, get, requests, paging} = setup({count: 60, pages: [10, 10]});
    buttons[59].handlers.click();
    assert.equal(get('zip-gallery-next').disabled, false, 'archive sentinel must keep next available');
    get('zip-gallery-next').onclick();
    await tick();
    assert.equal(paging.calls, 1);
    assert.equal(get('zip-gallery-title').textContent, '60.jpg');
    assert.equal(get('zip-gallery-counter').textContent, '61 / 70');
    assert.ok(requests.some(request => request.url === '/member/60'));
    get('zip-gallery-close').onclick(); await tick();
});

test('Go gallery jump crosses archive pages and stops once the requested image exists', async () => {
    const {buttons, get, paging} = setup({count: 60, pages: [10, 10, 10]});
    buttons[0].handlers.click();
    get('zip-gallery-jump-input').value = '75';
    await get('zip-gallery-jump-form').handlers.submit({preventDefault() {}});
    assert.equal(paging.calls, 2);
    assert.equal(get('zip-gallery-title').textContent, '74.jpg');
    assert.equal(get('zip-gallery-counter').textContent, '75 / 80');
    get('zip-gallery-close').onclick(); await tick();
});

test('closing during archive pagination prevents a late reply reopening or repainting the gallery', async () => {
    const {buttons, get, paging, requests} = setup({count: 60, pages: [10], delayedPage: true});
    buttons[59].handlers.click();
    get('zip-gallery-next').onclick();
    get('zip-gallery-close').onclick();
    paging.pending.shift()(); await tick();
    assert.equal(get('zip-gallery').open, false);
    assert.equal(requests.length, 1, 'late metadata must not start an image request');
    assert.equal(get('zip-gallery-stage').children.filter(node => node.src).length, 0);
});


test('closing during a progressive archive wait cancels the gallery poll timer', async () => {
    const {buttons, get, paging, timers} = setup({count: 60, pages: [0, 10], manualTimers: true});
    buttons[59].handlers.click();
    get('zip-gallery-next').onclick(); await tick();
    assert.equal(paging.calls, 1);
    assert.equal(timers.size, 1);
    get('zip-gallery-close').onclick(); await tick();
    assert.equal(timers.size, 0);
    assert.equal(paging.calls, 1, 'closed dialog must not consume another page');
});


test('closing while EXIF is being read releases the blob and cannot repopulate a new gallery cache', async () => {
    const {buttons, get, requests, exif, revoked} = setup({count: 1, delayedExif: true});
    buttons[0].handlers.click(); requests[0].resolve(); await tick();
    assert.equal(exif.length, 1);
    get('zip-gallery-close').onclick();
    exif.shift()(null); await tick();
    assert.deepEqual(revoked, ['blob:1']);
    buttons[0].handlers.click();
    assert.equal(requests.length, 2, 'the cancelled old image must not populate the reopened cache');
    get('zip-gallery-close').onclick(); await tick();
});


test('ArrowRight and swipe beyond the last slide keep the current pending image generation valid', async () => {
    const {buttons, get, requests, images} = setup({count: 1});
    buttons[0].handlers.click();
    get('zip-gallery').handlers.keydown({key: 'ArrowRight', preventDefault() {}});
    get('zip-gallery-stage').handlers.touchstart({touches: [{clientX: 200}]});
    get('zip-gallery-stage').handlers.touchend({changedTouches: [{clientX: 20}]});
    requests[0].resolve(); await tick();
    assert.equal(images.length, 1);
    assert.equal(images[0].alt, '0.jpg');
    images[0].onload();
    assert.match(get('zip-gallery-status').textContent, /滚轮/);
    get('zip-gallery-close').onclick(); await tick();
});

test('a successful final page containing only nonimages exhausts Next without reporting a load failure', async () => {
    const {buttons, get, requests, images, timers, paging} = setup({count: 60, pages: [0], manualTimers: true});
    buttons[59].handlers.click();
    get('zip-gallery-next').onclick(); await tick();
    assert.equal(paging.calls, 1);
    assert.equal(timers.size, 0, 'final page must not enter the progressive poll delay');
    assert.equal(get('zip-gallery-next').disabled, true);
    assert.equal(get('zip-gallery-counter').textContent, '60 / 60');
    assert.doesNotMatch(get('zip-gallery-status').textContent, /加载失败/);
    requests[0].resolve(); await tick();
    assert.equal(images.length, 1, 'end-of-list must preserve the pending current image');
    images[0].onload();
    assert.match(get('zip-gallery-status').textContent, /滚轮/);
    get('zip-gallery-close').onclick(); await tick();
});

test('current image may finish while the final metadata page loads, and its ready status survives exhaustion', async () => {
    const {buttons, get, requests, images, paging} = setup({count: 1, pages: [0], delayedPage: true});
    buttons[0].handlers.click(); get('zip-gallery-next').onclick();
    requests[0].resolve(); await tick();
    assert.equal(images.length, 1);
    images[0].onload();
    paging.pending.shift()(); await tick();
    assert.match(get('zip-gallery-status').textContent, /滚轮/);
    assert.equal(get('zip-gallery-next').disabled, true);
    get('zip-gallery-close').onclick(); await tick();
});


test('native jump validation allows later metadata pages and enforces max only at the final page', async () => {
    const {buttons, get, requests, images, paging} = setup({count: 60, pages: [10]});
    const input = get('zip-gallery-jump-input');
    get('zip-gallery-thumbs').hidden = true;
    input.max = '60';
    buttons[59].handlers.click(); requests[0].resolve(); await tick(); images[0].onload();
    assert.equal(input.getAttribute('max'), null, 'more pages must remove a stale native max even with thumbnails hidden');
    input.value = '61';
    assert.equal(input.checkValidity(), true, 'HTML number validation must allow the cross-page submit');
    await get('zip-gallery-jump-form').handlers.submit({preventDefault() {}});
    assert.equal(paging.calls, 1);
    assert.equal(get('zip-gallery-title').textContent, '60.jpg');
    requests.find(request => request.url === '/member/60').resolve(); await tick(); images.at(-1).onload();
    assert.equal(input.getAttribute('max'), '70', 'the complete image count becomes the native max');
    input.value = '71';
    assert.equal(input.checkValidity(), false);
    get('zip-gallery-close').onclick(); await tick();
});
