const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function events() {
    const handlers = new Map();
    return {
        addEventListener(name, fn) { if (!handlers.has(name)) handlers.set(name, new Set()); handlers.get(name).add(fn); },
        removeEventListener(name, fn) { handlers.get(name)?.delete(fn); },
        dispatchEvent(event) { for (const fn of [...handlers.get(event.type) || []]) fn(event); },
    };
}
const tick = () => new Promise(resolve => setImmediate(resolve));
function setup({cursor = 'A:60', complete = true} = {}) {
    const document = events(), window = events(), requests = [], observers = [], timers = new Map(), notices = [];
    let timerId = 0;
    const makeView = (name, next = `${name}:60`, done = true) => ({
        dialog: {dataset: {archiveEntriesUrl: `/archive/${name}?entries=1`, archiveNextCursor: next, archiveComplete: String(done)}},
        sentinel: {isConnected: true, remove() {this.isConnected = false;}},
        grid: {rows: [], insertAdjacentHTML(_, html) {this.rows.push(html);}},
        list: {rows: [], insertAdjacentHTML(_, html) {this.rows.push(html);}},
    });
    const state = {view: makeView('A', cursor, complete)};
    Object.assign(document, {
        getElementById: id => ({'zip-gallery': state.view.dialog, fileContainer: state.view.grid, fileListView: state.view.list})[id],
        querySelector: selector => selector === '[data-archive-sentinel]' && state.view.sentinel.isConnected ? state.view.sentinel : null,
        querySelectorAll: () => [],
    });
    document.addEventListener('files:load-complete', event => notices.push(event.detail));
    class IntersectionObserver {
        constructor(callback) { this.callback = callback; this.targets = new Set(); observers.push(this); }
        observe(target) {this.targets.add(target); this.observeCount = (this.observeCount || 0) + 1;}
        unobserve(target) {this.targets.delete(target);}
        disconnect() {this.targets.clear(); this.disconnected = true;}
        intersect(isIntersecting = true) {this.callback([{isIntersecting}]);}
    }
    window.IntersectionObserver = IntersectionObserver;
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/archive_entries.js', 'utf8'), {
        document, window, URL, location: {href: 'http://test/archive/A'}, AbortController, DOMException, IntersectionObserver,
        CustomEvent: class {constructor(type, options = {}) {this.type = type; this.detail = options.detail;}},
        setTimeout: (fn, delay) => {const id = ++timerId; timers.set(id, {fn, delay}); return id;},
        clearTimeout: id => timers.delete(id),
        // Deliberately let aborted requests complete: the owner check must
        // protect the new DOM even when a fetch implementation ignores abort.
        fetch: (url, options) => new Promise(resolve => requests.push({url: String(url), signal: options.signal,
            respond(page, ok = true) {resolve({ok, status: ok ? 200 : 500, headers: {get: () => null}, json: async () => page});}})),
    });
    return {...state, document, window, requests, observers, timers, notices,
        get view() {return state.view;},
        navigate(name) {
            document.dispatchEvent({type: 'cloudarchive:before-navigate'});
            state.view = makeView(name);
            document.dispatchEvent({type: 'cloudarchive:navigated'});
        },
        runPoll() { const [id, timer] = [...timers].sort((a, b) => a[1].delay - b[1].delay)[0]; timers.delete(id); timer.fn(); },
    };
}

test('a delayed archive reply cannot append to a new directory or overwrite its cursor', async () => {
    const s = setup(), oldView = s.view;
    const first = s.window.loadArchiveNextPage();
    s.navigate('B');
    const second = s.window.loadArchiveNextPage();
    assert.equal(s.requests[0].signal.aborted, true);
    assert.equal(s.observers[0].disconnected, true);
    s.requests[0].respond({items: [{href: '/A/late', name: 'late'}], next_cursor: 'A:120', complete: true});
    assert.equal(await first, false);
    assert.equal(s.view.grid.rows.length, 0);
    assert.equal(oldView.grid.rows.length, 0);
    assert.equal(s.notices.length, 0, 'late completion must not resolve another directory waiter');
    s.requests[1].respond({items: [{href: '/B/first', name: 'first'}], next_cursor: 'B:120', complete: true});
    assert.equal(await second, true);
    const third = s.window.loadArchiveNextPage();
    assert.equal(new URL(s.requests[2].url).searchParams.get('cursor'), 'B:120');
    s.requests[2].respond({items: [], complete: true});
    await third;
    assert.equal(s.view.grid.rows.length, 1);
    assert.equal(s.view.list.rows.length, 1);
    assert.equal(s.window.hasArchiveNextPage(), false);
});

test('a growing archive with no initial cursor waits with bounded backoff and cancels on navigation', async () => {
    const s = setup({cursor: '', complete: false});
    assert.equal(s.window.hasArchiveNextPage(), true);
    s.observers[0].intersect();
    assert.equal(new URL(s.requests[0].url).searchParams.get('cursor'), '');
    s.requests[0].respond({items: [], next_cursor: 'opaque:0', complete: false});
    await tick();
    assert.equal(s.observers[0].targets.size, 0, 'empty response must not immediately rearm the observer');
    assert.deepEqual([...s.timers.values()].map(t => t.delay).sort((a, b) => a - b), [1000, 90000]);
    s.runPoll();
    await tick();
    s.requests[1].respond({items: [], next_cursor: 'opaque:0', complete: false});
    await tick();
    assert.deepEqual([...s.timers.values()].map(t => t.delay).sort((a, b) => a - b), [2000, 90000]);
    s.navigate('B');
    assert.equal(s.timers.size, 0, 'navigation cancels every poll');
    s.observers[0].intersect();
    assert.equal(s.requests.length, 2, 'queued old observer callbacks cannot load the new directory');
});

test('gallery and observer reuse one archive page request, and manual retry supersedes scheduled retry', async () => {
    const s = setup();
    s.observers[0].intersect();
    const pending = s.window.loadArchiveNextPage();
    assert.equal(s.requests.length, 1);
    s.requests[0].respond({}, false);
    assert.equal(await pending, false);
    assert.equal(s.timers.size, 1);
    const retry = s.window.loadArchiveNextPage();
    s.requests[1].respond({items: [{href: '/A/a', name: 'a'}, {href: '/A/a', name: 'a'}], complete: true});
    assert.equal(await retry, true);
    assert.equal(s.view.grid.rows.length, 1);
    assert.equal(s.view.list.rows.length, 1);
});



test('navigation from a page-complete listener cannot leave an old archive poll behind', async () => {
    const s = setup({complete: false});
    s.document.addEventListener('files:load-complete', () => s.navigate('B'));
    const pending = s.window.loadArchiveNextPage();
    s.requests[0].respond({items: [], complete: true});
    await pending;
    assert.equal(s.timers.size, 0);
    assert.equal(s.observers[0].targets.size, 0);
    assert.equal(s.observers[0].disconnected, true);
    assert.equal(s.window.hasArchiveNextPage(), true);
});


test('archive sentinel automatically retries transient failures with bounded exponential backoff', async () => {
    const s = setup();
    s.observers[0].intersect();
    for (let attempt = 0; attempt < 4; attempt++) {
        s.requests[attempt].respond({}, false); await tick();
        if (attempt < 3) {
            assert.equal(s.timers.size, 1);
            assert.equal([...s.timers.values()][0].delay, 1000 * 2 ** attempt);
            s.runPoll();
        }
    }
    assert.equal(s.timers.size, 0);
    assert.equal(s.requests.length, 4);
    s.observers[0].intersect();
    assert.equal(s.requests.length, 4, 'visible callback must not defeat the retry cap');
    s.observers[0].intersect(false); s.observers[0].intersect(true);
    assert.equal(s.requests.length, 5, 'scrolling back into the preload area permits retry');
    s.requests[4].respond({items: [{href: '/A/recovered', name: 'recovered'}], complete: true}); await tick();
    assert.equal(s.view.grid.rows.length, 1);
    assert.equal(s.observers[0].disconnected, true);
});

test('archive transient retry timers are cancelled when navigating to a new session', async () => {
    const s = setup();
    s.observers[0].intersect(); s.requests[0].respond({}, false); await tick();
    assert.equal(s.timers.size, 1);
    s.navigate('B');
    assert.equal(s.timers.size, 0);
    assert.equal(s.requests.length, 1);
});

test('archive BFcache return resumes from the most recently appended cursor', async () => {
    const s = setup();
    const first = s.window.loadArchiveNextPage();
    s.requests[0].respond({items: [{href: '/A/one', name: 'one'}], next_cursor: 'A:120', complete: true});
    await first;
    s.window.dispatchEvent({type: 'pagehide'});
    assert.equal(s.window.hasArchiveNextPage(), false);
    s.window.dispatchEvent({type: 'pageshow', persisted: true});
    assert.equal(s.window.hasArchiveNextPage(), true);
    s.observers[1].intersect();
    assert.equal(new URL(s.requests[1].url).searchParams.get('cursor'), 'A:120');
    s.requests[1].respond({items: [{href: '/A/two', name: 'two'}], complete: true}); await tick();
    assert.equal(s.view.grid.rows.length, 2);
});


test('an already queued old archive retry callback cannot fetch from a new directory', async () => {
    const s = setup();
    s.observers[0].intersect(); s.requests[0].respond({}, false); await tick();
    const queued = [...s.timers.values()][0].fn;
    s.navigate('B'); queued();
    assert.equal(s.requests.length, 1);
});
