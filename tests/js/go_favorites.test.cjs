const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function element(tagName = 'DIV') {
    return {
        tagName, dataset: {}, children: [], style: {setProperty() {}}, classList: {toggle() {}},
        append(...children) { this.children.push(...children); },
        replaceChildren(...children) { this.children = children; },
        addEventListener() {}, setAttribute() {}, querySelector() { return null; }, querySelectorAll() { return []; },
        showModal() {}, close() {}, click() {},
    };
}

function renderFavorites(records) {
    const elements = new Map();
    const get = id => {
        if (!elements.has(id)) elements.set(id, element(id === 'favorite-picker' ? 'DIALOG' : 'DIV'));
        return elements.get(id);
    };
    const palette = get('favorite-colors');
    palette.querySelectorAll = () => palette.children;
    const storage = new Map([['cloudarchive.favorites.v1', JSON.stringify(records)]]);
    const document = {
        getElementById: get,
        createElement: tag => element(tag.toUpperCase()),
        querySelectorAll: () => [],
        addEventListener() {},
    };
    const location = {href: 'http://example.test/favorites', origin: 'http://example.test'};
    const window = {addEventListener() {}};
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/favorites.js', 'utf8'), {
        document, window, location, URL, Blob,
        localStorage: {getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value)},
        history: {replaceState() {}}, MutationObserver: class {observe() {}}, setTimeout,
    });
    return get('favorite-results').children;
}

test('Go archive favorites accept versioned path identities and keep their rendered open URL', () => {
    const memberPath = '漫画/第一:卷/章节:一/封面 图.jpg';
    const key = `zip:v2:42:${Buffer.from(memberPath).toString('base64url')}`;
    const url = '/archive/42/member?path=%E6%BC%AB%E7%94%BB%2F%E7%AC%AC%E4%B8%80%3A%E5%8D%B7%2F%E7%AB%A0%E8%8A%82%3A%E4%B8%80%2F%E5%B0%81%E9%9D%A2+%E5%9B%BE.jpg';
    const cards = renderFavorites({
        [key]: {name: '封面 图.jpg', url, memberPath, color: 'blue'},
        'zip:v2:42:not+base64': {name: 'invalid', url: '/archive/42/member?path=x', color: 'red'},
    });
    assert.equal(cards.length, 1, 'the v2 key is valid and malformed encodings are rejected');
    assert.equal(cards[0].children[0].href, `http://example.test${url}`);
});

test('legacy index favorites retain their explicit historical route conversion', () => {
    const cards = renderFavorites({
        'zip:7:revision-token:3': {name: 'old.jpg', url: '/file/7/zip', memberPath: 'old:folder/old.jpg', color: 'gray'},
    });
    assert.equal(cards.length, 1);
    assert.equal(cards[0].children[0].href, 'http://example.test/file/7/zip/member/3?v=revision-token&expected_path=old%3Afolder%2Fold.jpg');
});
