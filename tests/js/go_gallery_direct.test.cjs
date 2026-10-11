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
    return {images, buttons, get, requests, revoked, storage, indexRequests, dispatchClick:target=>(documentHandlers.click||[]).forEach(handler=>handler({target,preventDefault(){}}))};
}
const tick = () => new Promise(resolve => setImmediate(resolve));


test('direct prefetch retains the loaded image when navigating forward and back', async () => {
    const {buttons,get,images}=setup({count:3});
    buttons.forEach((b,i)=>b.dataset.url='/file/'+i+'/preview?direct=1');
    buttons[0].handlers.click();
    assert.equal(images.length,1);
    const first=images[0]; first.onload();
    await tick();
    assert.equal(get('zip-gallery-stage').children[0],first);
    assert.equal(first.assignments,1);
    const next=images.find(i=>i.url.includes('/file/1/'));
    assert.ok(next);
    next.onload(); await tick();
    get('zip-gallery-next').onclick(); await tick();
    assert.equal(get('zip-gallery-stage').children[0],next);
    assert.equal(next.assignments,1);
    get('zip-gallery-prev').onclick(); await tick();
    assert.equal(get('zip-gallery-stage').children[0],first);
    assert.equal(first.assignments,1);
    get('zip-gallery-close').onclick();
    await tick();
});
