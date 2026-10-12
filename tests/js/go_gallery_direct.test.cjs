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
    const requests = [], indexRequests = [], revoked = [], images = [];
    const storage = new Map();
    let blobId = 0;
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/zip_gallery.js', 'utf8'), {
        document: {
            querySelectorAll: () => buttons, getElementById: get, createElement: () => element(),
            querySelector: selector => selector === '[data-gallery-resume]' ? get('gallery-resume') : null,
            addEventListener: (name, handler) => { (documentHandlers[name] ||= []).push(handler); },
            removeEventListener: name => { delete documentHandlers[name]; },
        },
        localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key)},
        location: {href: 'http://localhost/browse', pathname: '/directory/0', search: ''},
        AbortController, DOMException, Image: class { constructor() { images.push(this); this.naturalWidth=100; this.naturalHeight=100; } set src(value) { this.assignments=(this.assignments||0)+1; this.url=value; } },
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
    return {images, buttons, get, requests, revoked, storage, indexRequests,
        dispatchClick:target=>(documentHandlers.click||[]).forEach(handler=>handler({target,preventDefault(){}})),
        dispatchDocument:(name, detail={})=>(documentHandlers[name]||[]).forEach(handler=>handler({detail}))};
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

test('Go gallery drops the previous page cache and uses the updated scope after partial navigation', async () => {
    const {buttons,get,requests,revoked,storage,dispatchDocument}=setup({count:2});
    buttons[0].handlers.click(); requests[0].resolve(); await tick();
    assert.deepEqual(JSON.parse(storage.get('galleryProgress:directory:0')), {identity:'0',name:'0.jpg'});

    get('zip-gallery').dataset.galleryScope='archive:9:pictures';
    dispatchDocument('cloudarchive:navigated', {url:'http://localhost/archive/9'});
    assert.deepEqual(revoked, ['blob:1']);

    const before = requests.filter(request => request.url === '/member/1').length;
    buttons[1].handlers.click();
    assert.equal(requests.filter(request => request.url === '/member/1').length, before + 1,
        'the new page must not reuse an index-based blob from the old page');
    assert.deepEqual(JSON.parse(storage.get('galleryProgress:archive:9:pictures')), {identity:'1',name:'1.jpg'});
});
