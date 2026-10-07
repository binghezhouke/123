/* One bounded raw-Deflate job per worker. No ZIP data is executed or written to disk. */
const MAX_VIDEO_BYTES = 64 * 1024 * 1024;
const CRC_TABLE = new Uint32Array(256);
for (let i = 0; i < 256; i++) {
    let crc = i;
    for (let bit = 0; bit < 8; bit++) crc = (crc & 1) ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
    CRC_TABLE[i] = crc >>> 0;
}
function userError(message) {
    const error = new Error(message);
    error.userMessage = message;
    return error;
}
let started = false, acknowledge = null;

self.onmessage = async ({data}) => {
    if (data?.type === 'ack') {acknowledge?.(); return;}
    if (data?.type !== 'start' || started) return;
    started = true;
    const controller = new AbortController();
    let reader, timer, timedOut = false;
    const touch = () => {
        clearTimeout(timer);
        timer = setTimeout(() => {timedOut = true; controller.abort();}, 60000);
    };
    try {
        const {size, compressedSize, crc32} = data;
        if (![size, compressedSize].every(n => Number.isSafeInteger(n) && n > 0 && n <= MAX_VIDEO_BYTES)
            || !Number.isSafeInteger(crc32) || crc32 < 0 || crc32 > 0xffffffff) {
            throw userError('视频大小或校验信息无效，单视频上限为 64 MiB。');
        }
        const url = new URL(data.url, self.location.href);
        if (url.origin !== self.location.origin || !['http:', 'https:'].includes(url.protocol)) {
            throw userError('视频地址无效，请刷新目录后重试。');
        }
        try {importScripts('vendor/pako_inflate.min.js');}
        catch {throw userError('无法加载客户端解压组件，请刷新页面后重试。');}
        const inflater = new pako.Inflate({raw: true, chunkSize: 64 * 1024});
        touch();
        const response = await fetch(url.href, {signal: controller.signal, credentials: 'same-origin'});
        if (response.status !== 200 || !response.body) {
            throw userError('视频读取失败，请重试；如果压缩包已变化，请刷新目录。');
        }
        const encoding = response.headers.get('Content-Encoding');
        const length = response.headers.get('Content-Length');
        if ((encoding && encoding !== 'identity') || (length !== null && Number(length) !== compressedSize)
            || !response.headers.get('Content-Type')?.startsWith('application/octet-stream')) {
            throw userError('视频响应与目录信息不一致，请刷新目录后重试。');
        }
        let received = 0, produced = 0, sent = 0, crc = 0xffffffff, lastProgress = 0;
        const progress = force => {
            const now = Date.now();
            if (force || now - lastProgress >= 150) {
                self.postMessage({type: 'progress', received, produced, total: compressedSize, size});
                lastProgress = now;
            }
        };
        // One bounded read loop: consume each network chunk before asking for another.
        // pako exposes stream completion and consumed bytes, unlike native decoders
        // that may accept trailing bytes in some environments.
        const output = new Uint8Array(size);
        inflater.onData = value => {
            if (produced + value.byteLength > size || produced + value.byteLength > MAX_VIDEO_BYTES) {
                throw userError('解压数据超过声明大小或 64 MiB 上限，已停止解压。');
            }
            output.set(value, produced);
            produced += value.byteLength;
            for (const byte of value) crc = CRC_TABLE[(crc ^ byte) & 0xff] ^ (crc >>> 8);
            progress(false);
        };
        reader = response.body.getReader();
        while (true) {
            const {done, value} = await reader.read();
            if (done) break;
            received += value.byteLength;
            if (received > compressedSize) throw userError('压缩数据超过声明大小，已停止读取。');
            if (inflater.ended) throw userError('压缩流结束后存在多余数据，请刷新目录后重试。');
            touch();
            inflater.push(value, received === compressedSize);
            if (inflater.err) throw userError('压缩数据损坏或不完整，请重试。');
            if (inflater.strm.avail_in !== 0) throw userError('压缩流结束后存在多余数据，请刷新目录后重试。');
            progress(false);
            if (data.stream) {
                // One transferred chunk at a time; SourceBuffer must consume it before
                // more output crosses the worker boundary. The full output remains
                // bounded for CRC verification and fallback playback.
                while (sent < produced) {
                    const end = Math.min(sent + 256 * 1024, produced);
                    const chunk = output.slice(sent, end).buffer;
                    clearTimeout(timer);
                    await new Promise((resolve, reject) => {
                        acknowledge = () => {clearTimeout(timer); acknowledge = null; resolve();};
                        timer = setTimeout(() => {acknowledge = null; reject(userError('播放器响应超时，请重试。'));}, 30000);
                        self.postMessage({type: 'chunk', buffer: chunk}, [chunk]);
                    });
                    sent = end;
                    touch();
                }
            }
        }
        if (!inflater.ended || inflater.err || inflater.strm.total_in !== compressedSize) {
            throw userError('压缩数据不完整，请重试。');
        }
        if (received !== compressedSize || produced !== size) throw userError('视频数据不完整，请重新加载。');
        if (((crc ^ 0xffffffff) >>> 0) !== crc32) throw userError('视频 CRC 校验失败，数据可能已损坏，请重试。');
        progress(true);
        const blob = new Blob([output], {type: 'video/mp4'});
        self.postMessage({type: 'done', blob});
    } catch (error) {
        self.postMessage({type: 'error', message: timedOut ? '视频读取超时，请重试。'
            : error.userMessage || '解压失败，数据可能不完整或损坏，请重试。'});
    } finally {
        clearTimeout(timer);
        controller.abort();
        if (reader) {try {await reader.cancel();} catch { /* Stream may already be errored. */ }}
        self.close();
    }
};
