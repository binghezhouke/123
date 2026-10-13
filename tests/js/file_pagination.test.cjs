const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function setup({go = false} = {}) {
    const item = id => ({dataset: {fileId: String(id)}});
    const grid = {items: [item(1)], querySelectorAll() {return this.items;}, append(fragment) {this.items.push(...fragment.items);}};
    const list = {...grid, items: [item(1)]};
    const button = {
        href: 'http://test/?last_file_id=1', attrs: {href: 'yes'}, dataset: {}, textContent: '',
        parentElement: {after(status) {state.status = status;}},
        closest() {return this.parentElement;},
        addEventListener(name, fn) {this.click = fn;},
        removeEventListener(name, fn) {if (this.click === fn) delete this.click;},
        hasAttribute(name) {return name in this.attrs;},
        setAttribute(name, value) {this.attrs[name] = value;},
        removeAttribute(name) {delete this.attrs[name];},
    };
    const heading = {classList: {contains: () => false}};
    const footer = {};
    const state = {button, grid, list, heading, footer, calls: [], signals: [], handlers: {}, scroller: {scrollTop: 120, scrollLeft: 0}, response: {ok: true, text: async () => 'page'}, ids: [1, 2], next: null};
    const document = {
        scrollingElement: state.scroller,
        querySelector: selector => selector === '[data-load-more]' ? button : selector === '[data-file-count]' ? heading : null,
        querySelectorAll: () => grid.items,
        getElementById: id => ({fileContainer: grid, fileListView: list, statusCount: footer})[id],
        createElement: () => ({setAttribute() {}}),
        createDocumentFragment: () => ({items: [], append(item) {this.items.push(item);}}),
        importNode: node => item(node.dataset.fileId),
        addEventListener(name, fn) {(state.handlers[name] ||= []).push(fn);},
        dispatchEvent(event) {(state.handlers[event.type] || []).forEach(fn => fn(event));},
    };
    class DOMParser {
        parseFromString() {
            return {
                querySelector: selector => selector === '[data-file-page]' ? {} : (state.next ? {href: state.next} : null),
                querySelectorAll: () => state.ids.map(item),
            };
        }
    }
    const intersections = [];
    state.intersections = intersections;
    class IntersectionObserver {
        constructor(callback) {this.callback = callback;}
        observe(target) {intersections.push(() => this.callback([{target, isIntersecting: true}]));}
        unobserve() {}
        disconnect() {intersections.length = 0;}
    }
    const windowHandlers = {};
    const window = {IntersectionObserver,
        addEventListener(name, fn) {(windowHandlers[name] ||= []).push(fn);},
        dispatchEvent(event) {(windowHandlers[event.type] || []).forEach(fn => fn(event));},
    };
    state.window = window; state.document = document;
    vm.runInNewContext(fs.readFileSync(go ? 'mount123/internal/webserve/static/js/file_pagination.js' : 'static/js/file_pagination.js', 'utf8'), {
        window, IntersectionObserver, AbortController,
        CustomEvent: class {constructor(type, options) {this.type = type; this.detail = options.detail;}},
        document, DOMParser, fetch: async (url, options) => {state.calls.push(url); state.signals.push(options.signal); return state.response;},
    });
    state.click = () => button.click({preventDefault() {}});
    return state;
}

test('appends distinct entries to both views and updates counts at the end', async () => {
    const s = setup();
    const original = s.grid.items[0];
    await s.click();
    assert.equal(s.grid.items[0], original);
    assert.deepEqual(s.grid.items.map(x => x.dataset.fileId), ['1', '2']);
    assert.deepEqual(s.list.items.map(x => x.dataset.fileId), ['1', '2']);
    assert.equal(s.heading.textContent, '2 个项目');
    assert.equal(s.footer.textContent, '2 个项目');
    assert.equal(s.button.hasAttribute('href'), false);
    await s.click();
    assert.equal(s.calls.length, 1);
});

test('uses the returned cursor, keeps empty intermediate pages pageable', async () => {
    const s = setup();
    s.ids = [];
    s.next = 'http://test/?last_file_id=2';
    await s.click();
    assert.equal(s.grid.items.length, 1);
    assert.equal(s.button.href, s.next);
    s.ids = [3];
    s.next = null;
    await s.click();
    assert.deepEqual(s.calls, ['http://test/?last_file_id=1', 'http://test/?last_file_id=2']);
    assert.equal(s.grid.items.length, 2);
});

test('keeps existing entries and cursor on error, allowing retry', async () => {
    const s = setup();
    s.response.ok = false;
    await s.click();
    assert.equal(s.grid.items.length, 1);
    assert.match(s.status.textContent, /加载失败/);
    s.response.ok = true;
    await s.click();
    assert.equal(s.grid.items.length, 2);
    assert.equal(s.calls[0], s.calls[1]);
});

test('only one request is active while loading', async () => {
    const s = setup();
    let release;
    s.response.text = () => new Promise(resolve => {release = resolve;});
    const pending = s.click();
    await Promise.resolve();
    await s.click();
    assert.equal(s.calls.length, 1);
    release('page');
    await pending;
});

test('repeated cursors are rejected without appending data', async () => {
    const s = setup();
    s.next = s.button.href;
    await s.click();
    assert.equal(s.grid.items.length, 1);
    assert.match(s.status.textContent, /加载失败/);
});


test('restores scroll when the browser anchors to the moving load button', async () => {
    const s = setup();
    const append = s.grid.append.bind(s.grid);
    s.grid.append = fragment => {
        append(fragment);
        s.scroller.scrollTop = 900;
    };
    await s.click();
    assert.equal(s.scroller.scrollTop, 120);
});


test('Go pagination keeps loading while sentinel remains inside preload margin', async () => {
    const s = setup({go: true});
    s.next = 'http://test/?offset=40';
    s.intersections.shift()();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(s.calls.length, 1);
    assert.equal(s.intersections.length, 1, 'visible sentinel must be checked again after append');
    s.ids = [3];
    s.next = null;
    s.intersections.shift()();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(s.calls.length, 2);
    assert.equal(s.grid.items.length, 3);
    assert.equal(s.intersections.length, 0, 'stop observing at final page');
});


test('Go directory pagination aborts and isolates late responses when the mounted directory changes', async () => {
    const s = setup({go: true});
    let release;
    s.response.text = () => new Promise(resolve => {release = resolve;});
    const oldRequest = s.window.loadNextFilePage();
    await new Promise(resolve => setImmediate(resolve));
    s.document.dispatchEvent({type: 'cloudarchive:before-navigate'});
    assert.equal(s.signals[0].aborted, true);
    const newButton = {...s.button, dataset: {}, attrs: {href: 'yes'}, href: 'http://test/?offset=40'};
    const newGrid = {...s.grid, items: [{dataset: {fileId: '10'}}]};
    const newList = {...s.list, items: [{dataset: {fileId: '10'}}]};
    s.document.querySelector = selector => selector === '[data-load-more]' ? newButton : null;
    s.document.querySelectorAll = () => newGrid.items;
    s.document.getElementById = id => ({fileContainer: newGrid, fileListView: newList})[id];
    s.document.dispatchEvent({type: 'cloudarchive:navigated'});
    release('old page');
    assert.equal(await oldRequest, false);
    assert.deepEqual(newGrid.items.map(item => item.dataset.fileId), ['10']);
    assert.deepEqual(s.grid.items.map(item => item.dataset.fileId), ['1']);
    s.response.text = async () => 'new page';
    s.ids = [10, 11];
    assert.equal(await s.window.loadNextFilePage(), true);
    assert.deepEqual(newGrid.items.map(item => item.dataset.fileId), ['10', '11']);
    assert.equal(s.window.hasNextFilePage(), false);
});


test('Go directory pagination resumes after a BFcache return without stale binding or cursor reset', async () => {
    const s = setup({go: true});
    s.next = 'http://test/?offset=40';
    assert.equal(await s.window.loadNextFilePage(), true);
    assert.equal(s.button.href, s.next);
    s.window.dispatchEvent({type: 'pagehide'});
    assert.equal(s.window.hasNextFilePage(), false);
    assert.equal(s.button.dataset.paginationBound, undefined);
    assert.equal(s.button.click, undefined);
    s.window.dispatchEvent({type: 'pageshow', persisted: true});
    assert.equal(s.button.dataset.paginationBound, '1');
    assert.equal(s.window.hasNextFilePage(), true);
    s.ids = [3]; s.next = null;
    assert.equal(await s.window.loadNextFilePage(), true);
    assert.equal(s.calls[1], 'http://test/?offset=40');
    assert.deepEqual(s.grid.items.map(item => item.dataset.fileId), ['1', '2', '3']);
    assert.equal(s.window.hasNextFilePage(), false);
});
