'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(process.argv[2], 'utf8');

class Clock {
    constructor() { this.now = 0; this.nextID = 1; this.timers = new Map(); }
    set(fn, delay) { const id = this.nextID++; this.timers.set(id, {fn, at: this.now + delay, delay}); return id; }
    clear(id) { this.timers.delete(id); }
    async next() {
        const next = [...this.timers].sort((a, b) => a[1].at - b[1].at)[0];
        assert.ok(next, 'expected a scheduled retry');
        this.timers.delete(next[0]); this.now = next[1].at; next[1].fn();
        await flush();
    }
}
async function flush() { for (let i = 0; i < 30; i++) await Promise.resolve(); }
const response = (status, page, retry = null) => ({status, ok: status >= 200 && status < 300,
    headers: {get: key => key.toLowerCase() === 'retry-after' ? retry : null}, json: async () => page});
const pending = retry => response(202, {code: 'archive_index_pending', retryable: true, items: [], complete: false, next_cursor: 'same-cursor'}, retry);
const row = {name: 'synthetic.txt', href: '/archive/1/member?path=synthetic.txt', is_dir: false};

function browser(replies) {
    const clock = new Clock(), events = [], requests = [], listeners = new Map();
    const element = () => ({dataset: {}, isConnected: true, html: [], insertAdjacentHTML(position, html) { this.html.push(html); }, remove() { this.isConnected = false; }});
    const sentinel = element(), dialog = element(), grid = element(), list = element(), status = element(), empty = element(), emptyTitle = element();
    dialog.dataset = {archiveEntriesUrl: '/archive/1/entries?path=', archiveNextCursor: 'same-cursor', archiveComplete: 'false'};
    const byID = {'zip-gallery': dialog, fileContainer: grid, fileListView: list, statusText: status};
    const document = {
        getElementById: id => byID[id] || null,
        querySelector: selector => selector === '[data-archive-sentinel]' ? sentinel : (selector === '.browse-files .empty-state' ? empty : (selector === '.browse-files .empty-state-title' ? emptyTitle : null)),
        querySelectorAll: () => [],
        addEventListener(type, fn) { const list = listeners.get(type) || []; list.push(fn); listeners.set(type, list); },
        dispatchEvent(event) { events.push(event); for (const fn of listeners.get(event.type) || []) fn(event); },
    };
    class Observer { observe() {} unobserve() {} disconnect() {} }
    const window = {IntersectionObserver: Observer, addEventListener: (...args) => document.addEventListener(...args)};
    class FakeDate extends Date { static now() { return clock.now; } }
    const fetch = async (url, options) => {
        requests.push({url: String(url), signal: options.signal});
        const next = replies.shift();
        assert.ok(next, 'unexpected additional request');
        return typeof next === 'function' ? next(options) : next;
    };
    vm.runInNewContext(source, {document, window, location: {href: 'https://synthetic.invalid/archive/1'}, URL, AbortController, DOMException,
        Date: FakeDate, performance: {now: () => clock.now}, fetch, IntersectionObserver: Observer,
        CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options?.detail; } },
        setTimeout: (fn, delay) => clock.set(fn, delay), clearTimeout: id => clock.clear(id), console});
    return {clock, window, document, grid, list, status, sentinel, dialog, empty, emptyTitle, events, requests};
}

(async () => {
    const h = browser([pending('2'), response(200, {items: [row], complete: true, next_cursor: ''})]);
    const task = h.window.loadArchiveNextPage();
    assert.equal(h.window.loadArchiveNextPage(), task, 'gallery and scroll share the in-flight index wait');
    await flush();
    assert.equal(h.requests.length, 1);
    assert.ok([...h.clock.timers.values()].some(timer => timer.delay === 2000), 'pending Retry-After must pace the next attempt');
    await h.clock.next();
    assert.equal(await task, true, 'the loading request resumes when metadata becomes ready');
    assert.equal(h.requests.length, 2);
    assert.equal(new URL(h.requests[1].url).searchParams.get('cursor'), 'same-cursor');
    assert.equal(h.grid.html.length, 1);
    assert.equal(h.list.html.length, 1);
    assert.equal(h.sentinel.isConnected, false);
    assert.equal(h.status.textContent, '就绪', 'ready metadata cannot leave the loading status behind');
    assert.equal(h.events.filter(event => event.type === 'files:load-complete' && event.detail.ok === false).length, 0);
    assert.equal(h.empty.isConnected, false, 'ready members replace the pending empty shell');

    // Progressive empty prefixes share the same bounded wait and preserve the
    // server's cursor; they must not erase members from an earlier page.
    const growing = browser([response(200, {items: [], complete: false, next_cursor: 'new-cursor'}), response(200, {items: [row], complete: true, next_cursor: ''})]);
    const growingTask = growing.window.loadArchiveNextPage();
    await flush(); await growing.clock.next();
    assert.equal(await growingTask, true);
    assert.equal(new URL(growing.requests[1].url).searchParams.get('cursor'), 'new-cursor');

    const nextRow = {...row, name: 'next.txt', href: '/archive/1/member?path=next.txt'};
    const retained = browser([response(200, {items: [row], complete: false, next_cursor: 'next-cursor'}), pending('1'),
        response(200, {items: [row, nextRow], complete: true, next_cursor: ''})]);
    assert.equal(await retained.window.loadArchiveNextPage(), true);
    const retainedTask = retained.window.loadArchiveNextPage();
    await flush();
    assert.equal(retained.grid.html.length, 1, 'pending metadata retains existing members');
    await retained.clock.next();
    assert.equal(await retainedTask, true);
    assert.equal(new URL(retained.requests[2].url).searchParams.get('cursor'), 'next-cursor');
    assert.equal(retained.grid.html.length, 2, 'resumed pages append without duplicating existing members');
    assert.equal(retained.list.html.length, 2);

    // Empty completed directories are distinct from pending indexes.
    const empty = browser([response(200, {items: [], complete: true, next_cursor: ''})]);
    assert.equal(await empty.window.loadArchiveNextPage(), true);
    assert.equal(empty.emptyTitle.textContent, '文件夹为空');

    // Repeated pending replies stop without an unbounded paced poll.
    const bounded = browser(Array.from({length: 10}, () => pending(null)));
    let boundedDone = false;
    const boundedTask = bounded.window.loadArchiveNextPage().then(result => {boundedDone = true; return result;});
    await flush();
    for (let i = 0; i < 12 && !boundedDone; i++) await bounded.clock.next();
    assert.equal(await boundedTask, false);
    assert.ok(bounded.requests.length <= 8, 'pending attempts must be bounded');
    assert.ok(bounded.clock.now < 90000, 'pending wait must stay within its time budget');
    assert.equal(bounded.clock.timers.size, 0, 'exhausted waiting cannot schedule another automatic request');
    assert.equal(bounded.status.textContent, '目录仍在读取，请刷新后重试', 'an empty shell cannot ask for an impossible scroll action');

    const boundedRows = browser([response(200, {items: [row], complete: false, next_cursor: 'next-cursor'}),
        ...Array.from({length: 8}, () => pending(null))]);
    assert.equal(await boundedRows.window.loadArchiveNextPage(), true);
    let boundedRowsDone = false;
    const boundedRowsTask = boundedRows.window.loadArchiveNextPage().then(result => {boundedRowsDone = true; return result;});
    await flush();
    for (let i = 0; i < 8 && !boundedRowsDone; i++) await boundedRows.clock.next();
    assert.equal(await boundedRowsTask, false);
    assert.equal(boundedRows.grid.html.length, 1);
    assert.equal(boundedRows.status.textContent, '正在读取目录，滚动后重试');
    assert.equal(boundedRows.clock.timers.size, 0);

    // A legitimate Retry-After which exceeds the remaining budget causes a
    // bounded stop instead of an earlier request against the server's advice.
    const longDelay = browser([pending('120')]);
    assert.equal(await longDelay.window.loadArchiveNextPage(), false);
    assert.equal(longDelay.requests.length, 1);
    assert.equal(longDelay.clock.timers.size, 0);

    // Navigation cancels both a paced wait and any currently active fetch.
    const leaving = browser([pending('2')]);
    const leavingTask = leaving.window.loadArchiveNextPage();
    await flush();
    leaving.document.dispatchEvent({type: 'cloudarchive:before-navigate'});
    assert.equal(leaving.clock.timers.size, 0, 'navigation synchronously cancels waiting and deadline timers');
    assert.equal(await leavingTask, false);
    assert.equal(leaving.requests[0].signal.aborted, true);
    assert.equal(leaving.clock.timers.size, 0);
    assert.equal(leaving.requests.length, 1);
    assert.equal(leaving.events.filter(event => event.type === 'files:load-complete').length, 0);

    const inflight = browser([options => new Promise((resolve, reject) => options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), {once: true}))]);
    const inflightTask = inflight.window.loadArchiveNextPage();
    await flush();
    inflight.document.dispatchEvent({type: 'pagehide'});
    assert.equal(await inflightTask, false);
    assert.equal(inflight.requests[0].signal.aborted, true);
    assert.equal(inflight.clock.timers.size, 0);

    // Real client errors never enter the pending or generic retry loop.
    for (const status of [400, 401, 403]) {
        const terminal = browser([response(status, {})]);
        assert.equal(await terminal.window.loadArchiveNextPage(), false);
        assert.equal(terminal.clock.timers.size, 0);
        assert.equal(terminal.requests.length, 1);
    }
    // A hung fetch also respects the total wait bound.
    const timed = browser([options => new Promise((resolve, reject) => options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), {once: true}))]);
    const timedTask = timed.window.loadArchiveNextPage();
    await flush(); await timed.clock.next();
    assert.equal(await timedTask, false);
    assert.equal(timed.clock.now, 90000);
    assert.equal(timed.requests[0].signal.aborted, true);
    assert.equal(timed.clock.timers.size, 0);

    // Navigation may race a response already enqueued by the network. It must
    // not add old members or completion events to the replacement session.
    let lateResponse;
    const late = browser([() => new Promise(resolve => { lateResponse = resolve; })]);
    const lateTask = late.window.loadArchiveNextPage();
    await flush();
    late.document.dispatchEvent({type: 'cloudarchive:before-navigate'});
    assert.equal(late.clock.timers.size, 0, 'a late network response cannot retain the old deadline timer');
    late.window.initArchiveEntries();
    lateResponse(response(200, {items: [row], complete: true, next_cursor: ''}));
    assert.equal(await lateTask, false);
    assert.equal(late.grid.html.length, 0);
    assert.equal(late.events.filter(event => event.type === 'files:load-complete').length, 0);

    for (const retry of ['invalid-date', '-2']) {
        const fallback = browser([pending(retry), response(200, {items: [row], complete: true, next_cursor: ''})]);
        const fallbackTask = fallback.window.loadArchiveNextPage();
        await flush();
        assert.ok([...fallback.clock.timers.values()].some(timer => timer.delay === 1000));
        await fallback.clock.next();
        assert.equal(await fallbackTask, true);
    }
    const date = browser([pending('Thu, 01 Jan 1970 00:00:02 GMT'), response(200, {items: [row], complete: true, next_cursor: ''})]);
    const dateTask = date.window.loadArchiveNextPage();
    await flush();
    assert.ok([...date.clock.timers.values()].some(timer => timer.delay === 2000));
    await date.clock.next();
    assert.equal(await dateTask, true);

    const malformed = browser([response(202, {retryable: false})]);
    assert.equal(await malformed.window.loadArchiveNextPage(), false);
    assert.equal(malformed.requests.length, 1);
    assert.equal(malformed.clock.timers.size, 0);
    console.log('archive pending browser tests passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
