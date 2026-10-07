const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');

class Events {
    constructor() {this.listeners = {};}
    addEventListener(name, fn) {(this.listeners[name] ||= []).push(fn);}
    emit(name) {for (const fn of this.listeners[name] || []) fn();}
}
function setup({supported = true, failAppend = false, delayed = false} = {}) {
    const sources = [], revoked = [];
    class Media extends Events {
        static isTypeSupported() {return supported;}
        constructor() {super(); this.readyState = 'open'; this.buffers = []; sources.push(this); setImmediate(() => this.emit('sourceopen'));}
        addSourceBuffer(type) {
            const buffer = new Events();
            Object.assign(buffer, {type, updating: false, chunks: [], abort() {this.updating = false;},
                appendBuffer(data) {
                    if (failAppend) throw new Error('QuotaExceededError');
                    this.chunks.push(data); this.updating = true;
                    if (!delayed) setImmediate(() => {this.updating = false; this.emit('updateend');});
                }});
            this.buffers.push(buffer); return buffer;
        }
        endOfStream() {this.readyState = 'ended';}
    }
    global.MediaSource = Media;
    global.URL.createObjectURL = () => 'blob:stream';
    global.URL.revokeObjectURL = url => revoked.push(url);
    const player = {src: '', pause() {}, load() {}, removeAttribute() {this.src = '';}};
    return {player, sources, revoked};
}
const fixture = fs.readFileSync('tests/fixtures/video/faststart.mp4');
const bytes = b => b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);

test('real MP4 produces audio/video segments before EOF and ends only after queued appends', async () => {
    const {createVideoStream} = await import('../../static/js/zip_video_stream.mjs');
    const {player, sources, revoked} = setup();
    const stream = createVideoStream(player, () => assert.fail('unexpected fallback'));
    await stream.append(bytes(fixture.subarray(0, Math.floor(fixture.length / 2))));
    assert.equal(stream.usable, true);
    assert.equal(sources[0].buffers.length, 2);
    assert.ok(sources[0].buffers.every(b => b.chunks.length >= 2));
    await stream.append(bytes(fixture.subarray(Math.floor(fixture.length / 2))));
    await stream.finish();
    assert.equal(sources[0].readyState, 'ended');
    stream.dispose(); stream.dispose();
    assert.deepEqual(revoked, ['blob:stream']);
    assert.equal(player.src, '');
});

test('unsupported codecs and SourceBuffer failures fall back without stopping download', async () => {
    const {createVideoStream} = await import('../../static/js/zip_video_stream.mjs');
    for (const options of [{supported: false}, {failAppend: true}]) {
        let fallbacks = 0;
        const {player} = setup(options);
        const stream = createVideoStream(player, () => fallbacks++);
        await stream.append(bytes(fixture));
        await stream.finish();
        assert.equal(fallbacks, 1);
        assert.equal(stream.usable, false);
    }
});

test('closing while SourceBuffer is busy releases the pending acknowledgement', async () => {
    const {createVideoStream} = await import('../../static/js/zip_video_stream.mjs');
    const {player} = setup({delayed: true});
    const stream = createVideoStream(player, () => assert.fail('cancel is not failure'));
    const append = stream.append(bytes(fixture));
    await new Promise(r => setImmediate(r));
    stream.dispose();
    await append;
    assert.equal(stream.usable, false);
});

test('no front index falls back after a bounded prefix', async () => {
    const {createVideoStream} = await import('../../static/js/zip_video_stream.mjs');
    const {player} = setup(); let fallbacks = 0;
    const stream = createVideoStream(player, () => fallbacks++);
    const mdat = Buffer.alloc(1024 * 1024); mdat.writeUInt32BE(2 * 1024 * 1024); mdat.write('mdat', 4);
    await stream.append(bytes(mdat));
    assert.equal(fallbacks, 1);
    assert.equal(stream.usable, false);
});
