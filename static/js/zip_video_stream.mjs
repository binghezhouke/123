import {createFile} from './vendor/mp4box/mp4box.all.mjs';

// A disposable progressive player. Failure leaves the validated Blob path available.
export function createVideoStream(player, onFallback) {
    const media = new MediaSource();
    const objectUrl = URL.createObjectURL(media);
    let parser = createFile(), offset = 0, ready = false, failed = false, disposed = false;
    let pending = 0, waiter = null, timer = null;
    const tracks = new Map();
    function settle() {
        if (pending || !waiter) return;
        clearTimeout(timer);
        const resolve = waiter;
        waiter = null;
        resolve();
    }
    function dispose() {
        if (disposed) return;
        disposed = true;
        clearTimeout(timer);
        parser?.stop();
        parser = null;
        for (const {buffer, queue} of tracks.values()) {
            queue.length = 0;
            try {if (buffer.updating) buffer.abort();} catch { /* Already detached. */ }
        }
        tracks.clear();
        pending = 0;
        settle();
        if (player.src === objectUrl) {
            player.pause();
            player.removeAttribute('src');
            player.load();
        }
        URL.revokeObjectURL(objectUrl);
    }
    function fallback() {
        if (failed || disposed) return;
        failed = true;
        dispose();
        onFallback();
    }
    function pump(track) {
        if (disposed || track.buffer.updating || !track.queue.length) return;
        try {track.buffer.appendBuffer(track.queue.shift());}
        catch {fallback();}
    }
    function enqueue(id, data) {
        if (disposed) return;
        const track = tracks.get(id);
        pending++;
        track.queue.push(data);
        pump(track);
    }
    parser.onError = fallback;
    parser.onReady = info => {
        try {
            const selected = [info.videoTracks?.[0], info.audioTracks?.[0]].filter(Boolean);
            if (!info.videoTracks?.length || info.isFragmented || selected.some(t =>
                !MediaSource.isTypeSupported(`${t.video ? 'video' : 'audio'}/mp4; codecs="${t.codec}"`))) {
                fallback(); return;
            }
            ready = true;
            for (const t of selected) {
                const buffer = media.addSourceBuffer(`${t.video ? 'video' : 'audio'}/mp4; codecs="${t.codec}"`);
                const track = {buffer, queue: []};
                tracks.set(t.id, track);
                buffer.addEventListener('error', fallback);
                buffer.addEventListener('updateend', () => {
                    if (disposed) return;
                    pending--;
                    pump(track);
                    settle();
                });
                parser.setSegmentOptions(t.id, null, {nbSamples: 12, rapAlignement: false});
            }
            parser.onSegment = (id, user, data, sampleNumber) => {
                enqueue(id, data);
                parser?.releaseUsedSamples(id, sampleNumber);
            };
            for (const init of parser.initializeSegmentation('per-track')) enqueue(init.id, init.buffer);
            parser.start();
        } catch {fallback();}
    };
    // Wait for sourceopen before consuming bytes; cancellation also releases this wait.
    pending = 1;
    media.addEventListener('sourceopen', () => {if (!disposed) {pending--; settle();}}, {once: true});
    player.src = objectUrl;
    player.load();
    const drain = () => {
        if (!pending || disposed) return Promise.resolve();
        return new Promise(resolve => {
            waiter = resolve;
            timer = setTimeout(fallback, 15000);
        });
    };
    return {
        get usable() {return ready && !disposed;},
        fallback,
        dispose,
        async append(data) {
            await drain();
            if (disposed) return;
            try {
                data.fileStart = offset;
                offset += data.byteLength;
                parser.appendBuffer(data);
                // A tail moov needs the entire file: avoid retaining a second full copy.
                if (!ready && offset >= 1024 * 1024) fallback();
                await drain();
            } catch {fallback();}
        },
        async finish() {
            if (disposed) return;
            try {
                parser.flush();
                await drain();
                if (!ready) {fallback(); return;}
                if (!disposed && media.readyState === 'open') media.endOfStream();
            } catch {fallback();}
        },
    };
}
