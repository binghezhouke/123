const {test} = require('node:test');
const assert = require('node:assert/strict');
const {deflateRawSync} = require('node:zlib');
const fs = require('node:fs');
const vm = require('node:vm');

const content = Buffer.from('hello world');
const compressed = deflateRawSync(content);
async function run({payload = compressed, size = 11, compressedSize = payload.length, crc32 = 0x0d4a1185,
                    stream = false, libraryAvailable = true, status = 200, declareLength = true, url = '/raw', readError = false} = {}) {
    const messages = [];
    let fetches = 0, closed = false, signal;
    const self = {
        location: {href: 'http://test/static/worker.js', origin: 'http://test'},
        postMessage: message => {
            messages.push(message);
            if (message.type === 'chunk') queueMicrotask(() => self.onmessage({data: {type: 'ack'}}));
        },
        close: () => {closed = true;},
    };
    const fetch = async (url, options) => {
        fetches++;
        signal = options.signal;
        let offset = 0;
        return new Response(new ReadableStream({pull(controller) {
            if (readError) {controller.error(new Error('network failure')); return;}
            if (offset === payload.length) controller.close();
            else {controller.enqueue(new Uint8Array(payload.subarray(offset, offset + 3))); offset = Math.min(offset + 3, payload.length);}
        }}), {status, headers: {'Content-Type': 'application/octet-stream',
            ...(declareLength ? {'Content-Length': String(payload.length)} : {})}});
    };
    const context = vm.createContext({
        self, fetch, Blob, URL, AbortController,
        importScripts() {
            if (!libraryAvailable) throw new Error('unavailable');
            vm.runInContext(fs.readFileSync('static/js/vendor/pako_inflate.min.js', 'utf8'), context);
        },
        setTimeout, clearTimeout, Uint8Array, Uint32Array,
    });
    vm.runInContext(fs.readFileSync('static/js/zip_inflate_worker.js', 'utf8'), context);
    await self.onmessage({data: {type: 'start', url, size, compressedSize, crc32, stream}});
    assert.equal(closed, true);
    if (signal) assert.equal(signal.aborted, true);
    return {messages, fetches};
}

test('real raw Deflate is inflated and CRC-verified before creating a playable Blob', async () => {
    const {messages} = await run();
    assert.equal(messages.at(-1).type, 'done');
    assert.equal(await messages.at(-1).blob.text(), 'hello world');
    assert.equal(messages.at(-1).blob.type, 'video/mp4');
    assert.equal(messages.at(-2).received, compressed.length);
    assert.equal(messages.at(-2).produced, 11);
});

test('CRC mismatch is an error and never publishes a Blob', async () => {
    const {messages} = await run({crc32: 0});
    assert.equal(messages.at(-1).type, 'error');
    assert.match(messages.at(-1).message, /CRC/);
    assert.equal(messages.some(message => message.type === 'done'), false);
});

test('actual decompressed output cannot exceed the declared size', async () => {
    const {messages} = await run({size: 4});
    assert.equal(messages.at(-1).type, 'error');
    assert.match(messages.at(-1).message, /超过/);
});

test('truncated streams, trailing garbage and incorrect output lengths fail', async () => {
    for (const options of [{payload: compressed.subarray(0, -1)},
                          {payload: Buffer.concat([compressed, Buffer.from('extra')])}, {size: 12}]) {
        const {messages} = await run(options);
        assert.equal(messages.at(-1).type, 'error');
        assert.equal(messages.some(message => message.type === 'done'), false);
    }
});

test('compressed input length is enforced even without a Content-Length header', async () => {
    for (const compressedSize of [compressed.length - 1, compressed.length + 1]) {
        const {messages} = await run({compressedSize, declareLength: false});
        assert.equal(messages.at(-1).type, 'error');
    }
});

test('64 MiB cap, metadata and same-origin checks run before fetch', async () => {
    for (const options of [{size: 64 * 1024 * 1024 + 1}, {compressedSize: 64 * 1024 * 1024 + 1},
                          {size: -1}, {size: 2.5}, {crc32: -1}, {url: 'https://other.test/file'}]) {
        const {messages, fetches} = await run(options);
        assert.equal(messages.at(-1).type, 'error');
        assert.equal(fetches, 0);
    }
});

test('a missing inflate component returns an actionable error without fetching video data', async () => {
    const {messages, fetches} = await run({libraryAvailable: false});
    assert.match(messages.at(-1).message, /解压组件/);
    assert.equal(fetches, 0);
});

test('HTTP and network errors never publish a Blob', async () => {
    for (const options of [{status: 500}, {readError: true}]) {
        const {messages} = await run(options);
        assert.equal(messages.at(-1).type, 'error');
        assert.equal(messages.some(message => message.type === 'done'), false);
    }
});


test('progressive chunks precede completion, but a CRC failure never creates the final cache', async () => {
    for (const crc32 of [0x0d4a1185, 0]) {
        const {messages} = await run({stream: true, crc32});
        const chunks = messages.filter(m => m.type === 'chunk');
        assert.equal(chunks.length, 1);
        assert.equal(Buffer.from(chunks[0].buffer).toString(), 'hello world');
        assert.equal(messages.at(-1).type, crc32 ? 'done' : 'error');
        if (!crc32) assert.match(messages.at(-1).message, /CRC/);
    }
});
