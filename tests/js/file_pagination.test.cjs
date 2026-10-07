const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function setup() {
    const item = id => ({dataset: {fileId: String(id)}});
    const grid = {items: [item(1)], querySelectorAll() {return this.items;}, append(fragment) {this.items.push(...fragment.items);}};
    const list = {...grid, items: [item(1)]};
    const button = {
        href: 'http://test/?last_file_id=1', attrs: {href: 'yes'}, textContent: '',
        parentElement: {after(status) {state.status = status;}},
        addEventListener(name, fn) {this.click = fn;},
        hasAttribute(name) {return name in this.attrs;},
        setAttribute(name, value) {this.attrs[name] = value;},
        removeAttribute(name) {delete this.attrs[name];},
    };
    const heading = {classList: {contains: () => false}};
    const footer = {};
    const state = {button, grid, list, heading, footer, calls: [], scroller: {scrollTop: 120, scrollLeft: 0}, response: {ok: true, text: async () => 'page'}, ids: [1, 2], next: null};
    const document = {
        scrollingElement: state.scroller,
        querySelector: selector => selector === '[data-load-more]' ? button : selector === '[data-file-count]' ? heading : null,
        querySelectorAll: () => grid.items,
        getElementById: id => ({fileContainer: grid, fileListView: list, statusCount: footer})[id],
        createElement: () => ({setAttribute() {}}),
        createDocumentFragment: () => ({items: [], append(item) {this.items.push(item);}}),
        importNode: node => item(node.dataset.fileId),
        addEventListener() {},
        dispatchEvent() {},
    };
    class DOMParser {
        parseFromString() {
            return {
                querySelector: selector => selector === '[data-file-page]' ? {} : (state.next ? {href: state.next} : null),
                querySelectorAll: () => state.ids.map(item),
            };
        }
    }
    vm.runInNewContext(fs.readFileSync('static/js/file_pagination.js', 'utf8'), {
        CustomEvent: class {constructor(type, options) {this.type = type; this.detail = options.detail;}},
        document, DOMParser, fetch: async url => {state.calls.push(url); return state.response;},
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
