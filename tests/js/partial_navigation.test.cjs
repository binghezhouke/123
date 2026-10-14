const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const tick = () => new Promise(resolve => setImmediate(resolve));
function events() {
    const handlers = new Map();
    return {
        addEventListener(name, fn) {if (!handlers.has(name)) handlers.set(name, new Set()); handlers.get(name).add(fn);},
        removeEventListener(name, fn) {handlers.get(name)?.delete(fn);},
        dispatchEvent(event) {for (const fn of [...handlers.get(event.type) || []]) fn(event);},
    };
}
function setup({saved = null, initial = 'A', delayNavigation = false, delayPage = false} = {}) {
    const document = events(), window = events(), storage = new Map(), timers = new Map(), frames = [], requests = [];
    let nextTimer = 0;
    const location = {href: `http://test/browse?parent=${initial}`, origin: 'http://test', assign(url) {this.assigned = url;}};
    window.location = location;
    const scroller = () => ({_top: 0, _left: 0, scrollHeight: 1000, clientHeight: 500,
        get scrollTop() {return this._top;}, set scrollTop(value) {this._top = Math.max(0, Math.min(this.scrollHeight - this.clientHeight, value));},
        get scrollLeft() {return this._left;}, set scrollLeft(value) {this._left = value;},
    });
    const browse = Object.assign(scroller(), {replaceChildren(...nodes) {this.nodes = nodes; this.scrollHeight = 1000;}});
    const finder = Object.assign(scroller(), {classList: {add() {}, remove() {}}, querySelector(selector) {return selector === '.browse-files' ? browse : null;}});
    Object.assign(document, {title: initial,
        querySelector: selector => selector === '.finder-main' ? finder : selector === '.browse-files' ? browse : null,
        getElementById: () => null,
        importNode: node => node,
    });
    const key = name => `cloudarchive.nav-position:/browse?parent=${name}`;
    if (saved) storage.set(key('B'), JSON.stringify([[0, 0], [saved, 0]]));
    class DOMParser {
        parseFromString(html) {
            const incomingBrowse = {childNodes: [html]};
            const incomingFinder = {querySelector: selector => selector === '.browse-files' ? incomingBrowse : null};
            return {title: html, querySelector: selector => selector === '.finder-main' ? incomingFinder : null};
        }
    }
    const paging = {calls: 0, pending: []};
    window.hasNextFilePage = () => paging.calls < 20;
    window.loadNextFilePage = () => {
        paging.calls++;
        const append = () => {browse.scrollHeight += 1000; document.dispatchEvent({type: 'scroll'}); return true;};
        return delayPage ? new Promise(resolve => paging.pending.push(() => resolve(append()))) : Promise.resolve(append());
    };
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/partial_navigation.js', 'utf8'), {
        document, window, location, DOMParser, URL, AbortController,
        sessionStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value)},
        localStorage: {getItem: () => null},
        history: {pushState(_, __, url) {location.href = url;}},
        CustomEvent: class {constructor(type, options = {}) {this.type = type; this.detail = options.detail;}},
        setTimeout: (fn, delay) => {const id = ++nextTimer; timers.set(id, {fn, delay}); return id;},
        clearTimeout: id => timers.delete(id),
        requestAnimationFrame: fn => frames.push(fn),
        fetch: (url, {signal}) => {
            const htmlHeaders = {get: () => 'text/html; charset=utf-8'};
            if (!delayNavigation) return Promise.resolve({ok: true, headers: htmlHeaders, text: async () => new URL(url).searchParams.get('parent')});
            return new Promise(resolve => requests.push({url, signal, respond: html => resolve({ok: true, headers: htmlHeaders, text: async () => html}), respondRaw: resolve}));
        },
    });
    const navigate = name => document.dispatchEvent({type: 'click', button: 0, preventDefault() {}, target: {
        closest: selector => selector === 'a[href]' ? {href: `http://test/browse?parent=${name}`, hasAttribute: () => false} : null,
    }});
    return {document, window, storage, timers, frames, paging, browse, finder, location, requests, key, navigate,
        async frame() {assert.ok(frames.length, 'restore must schedule a frame'); frames.shift()(); await tick();},
    };
}

test('partial navigation reloads only enough metadata pages to reach a saved scroll offset', async () => {
    const s = setup({saved: 4000});
    s.navigate('B'); await tick();
    await s.frame();
    assert.equal(s.browse.scrollTop, 500, 'first page clamps the saved offset');
    assert.equal(s.paging.calls, 1);
    for (let i = 0; i < 4; i++) await s.frame();
    assert.equal(s.browse.scrollTop, 4000);
    assert.equal(s.paging.calls, 4, 'stop as soon as saved position fits');
    assert.equal(s.frames.length, 0);
    assert.equal(JSON.parse(s.storage.get(s.key('B')))[1][0], 4000, 'restoration scroll events must not overwrite the saved target');
    assert.equal(s.timers.size, 0);
});

test('without a saved position partial navigation resets scroll without consuming pages', async () => {
    const s = setup();
    s.browse.scrollTop = 300;
    s.navigate('B'); await tick(); await s.frame();
    assert.equal(s.browse.scrollTop, 0);
    assert.equal(s.paging.calls, 0);
});

test('popstate saves the outgoing mounted URL and cancels stale delayed position writes', async () => {
    const s = setup({saved: 4000});
    s.browse.scrollTop = 300;
    s.document.dispatchEvent({type: 'scroll'});
    assert.equal(s.timers.size, 1);
    s.location.href = 'http://test/browse?parent=B';
    s.window.dispatchEvent({type: 'popstate'});
    assert.equal(s.timers.size, 0);
    assert.equal(JSON.parse(s.storage.get(s.key('A')))[1][0], 300);
    assert.equal(JSON.parse(s.storage.get(s.key('B')))[1][0], 4000);
    await tick();
});

test('navigation cancels an unfinished restoration and its late page cannot restore the next view', async () => {
    const s = setup({saved: 4000, delayPage: true});
    s.navigate('B'); await tick(); await s.frame();
    assert.equal(s.paging.calls, 1);
    s.navigate('C'); await tick(); await s.frame();
    s.paging.pending.shift()(); await tick();
    assert.equal(s.browse.scrollTop, 0);
    assert.equal(s.frames.length, 0, 'cancelled restoration must not schedule another frame');
    assert.equal(JSON.parse(s.storage.get(s.key('B')))[1][0], 4000, 'clamped intermediate position must not replace target');
});

test('a delayed earlier navigation cannot overwrite the later completed directory', async () => {
    const s = setup({delayNavigation: true});
    s.navigate('B'); s.navigate('C');
    assert.equal(s.requests[0].signal.aborted, true);
    s.requests[1].respond('C'); await tick();
    s.requests[0].respond('B'); await tick();
    assert.equal(s.document.title, 'C');
    assert.equal(s.location.href, 'http://test/browse?parent=C');
    assert.deepEqual(s.browse.nodes, ['C']);
});


test('partial-navigation capture allows a directory load-more retry to use its own pagination handler', () => {
    const s = setup({delayNavigation: true});
    const link = {href: 'http://test/browse?parent=A&offset=40', hasAttribute: name => name === 'data-load-more'};
    let prevented = false, pageLoads = 0;
    s.document.addEventListener('click', event => { if (!prevented && event.target.closest('a[href]') === link) pageLoads++; });
    s.document.dispatchEvent({type: 'click', button: 0, preventDefault() {prevented = true;}, target: {
        closest: selector => selector === 'a[href]' ? link : null,
    }});
    assert.equal(pageLoads, 1);
    assert.equal(s.requests.length, 0, 'load-more must not start a full partial navigation');
    assert.equal(prevented, false);
});

test('archive member streams and metadata retain native navigation', () => {
    for (const suffix of ['/member?path=inner.zip%2Fclip.mp4', '/member?path=clip.mp4&download=1', '/entries', '/index', '/unlock']) {
        const s = setup({delayNavigation: true});
        const link = {href: `http://test/archive/7${suffix}`, hasAttribute: () => false};
        let prevented = false;
        s.document.dispatchEvent({type: 'click', button: 0, preventDefault() {prevented = true;}, target: {
            closest: selector => selector === 'a[href]' ? link : null,
        }});
        assert.equal(prevented, false, suffix);
        assert.equal(s.requests.length, 0, 'binary/API endpoints must not be fetched as HTML');
    }
});

test('nested archive HTML pages still use partial navigation', () => {
    const s = setup({delayNavigation: true});
    const link = {href: 'http://test/archive/7?path=inner.zip%2Ffolder', hasAttribute: () => false};
    let prevented = false;
    s.document.dispatchEvent({type: 'click', button: 0, preventDefault() {prevented = true;}, target: {
        closest: selector => selector === 'a[href]' ? link : null,
    }});
    assert.equal(prevented, true);
    assert.equal(s.requests.length, 1);
});

test('a non-HTML response is cancelled before its binary body can be read as text', async () => {
    const s = setup({delayNavigation: true});
    let reads = 0, cancellations = 0;
    s.navigate('B');
    s.requests[0].respondRaw({ok: true, headers: {get: () => 'video/mp4'},
        body: {cancel: async () => {cancellations++;}},
        text: async () => {reads++; return 'binary';},
    });
    await tick();
    assert.equal(reads, 0);
    assert.equal(cancellations, 1);
    assert.equal(s.location.assigned, 'http://test/browse?parent=B');
});
