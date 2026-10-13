const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const node = () => ({
    hidden: false, style: {}, dataset: {}, textContent: '',
    classList: {add() {}, remove() {}}, setAttribute() {}, removeAttribute() {},
    replaceChildren() {}, append() {}, addEventListener() {},
});
const root = node(), pane = node(), empty = node();
pane.querySelector = () => empty;
const elements = Object.fromEntries(['preview-stage', 'preview-title', 'preview-meta',
    'preview-actions', 'preview-open', 'preview-download', 'fileContainer', 'fileListView',
    'preview-fullscreen'].map(id => [id, node()]));
elements['preview-pane'] = pane;
const listeners = new Map();
let items = [];
const document = {
    querySelector: selector => selector === '.browse-files' ? root : null,
    querySelectorAll: selector => selector === '.file-item' ? items : [],
    getElementById: id => elements[id] || null,
    createElement: node,
    addEventListener(type, fn) {
        const callbacks = listeners.get(type) || [];
        callbacks.push(fn); listeners.set(type, callbacks);
    },
};
const context = vm.createContext({document, URL, window: {location: {href: 'http://test/archive/7'}, addEventListener() {}},
    localStorage: {getItem() { return 'icons'; }, setItem() {}},
});
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), context);

// The document/listener survives navigation; replacing items models both the
// ordinary directory and archive views in this same page lifetime.
for (const view of ['icons', 'list']) {
    context.switchView(view);
    for (const [name, dataset, navigates] of [
        ['cloud ZIP after return', {fileType: 'file', kind: '压缩包', favoriteName: 'sample.zip', favoriteUrl: '/archive/7', fileId: '7'}, true],
        ['detected renamed archive', {fileType: 'file', kind: '压缩包', favoriteName: 'sample.bin', favoriteUrl: '/archive/7', fileId: '7'}, true],
        ['cloud directory', {fileType: 'folder', kind: '文件夹', favoriteName: 'folder', favoriteUrl: '/browse?parent_id=7', fileId: '7'}, true],
        ['nested archive', {fileType: 'folder', kind: '压缩包', favoriteName: 'inner.7z', favoriteUrl: '/archive/7?path=inner.7z', memberId: 'nested'}, true],
        ['archive directory', {fileType: 'folder', kind: '文件夹', favoriteName: 'folder', favoriteUrl: '/archive/7?path=folder', memberId: 'folder'}, true],
        ['cloud image', {fileType: 'file', kind: '图片', favoriteName: 'image.jpg', url: '/file/8/preview', fileId: '8'}, false],
        ['archive image', {fileType: 'file', kind: '图片', favoriteName: 'image.jpg', url: '/archive/7/member?path=image.jpg', memberId: 'image'}, false],
    ]) {
        const item = node(); item.dataset = dataset; items = [item];
        const event = {defaultPrevented: false,
            target: {closest: selector => selector === '.file-item' ? item : null},
            preventDefault() { this.defaultPrevented = true; },
        };
        for (const listener of listeners.get('click')) listener(event);
        assert.equal(!event.defaultPrevented, navigates, `${view}: ${name} navigation`);
        assert.equal(elements['preview-open'].href, dataset.favoriteUrl || dataset.url);
    }
}
const before = elements['preview-title'].textContent;
const favoriteEvent = {target: {closest: selector => selector === '.file-item' ? items[0] : node()},
    preventDefault() { throw new Error('favorite click intercepted'); }};
for (const listener of listeners.get('click')) listener(favoriteEvent);
assert.equal(elements['preview-title'].textContent, before);
console.log('PASS: archive/ordinary navigation, icon/list previews, favorite controls');
