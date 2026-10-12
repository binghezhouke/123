// Shares the complete image body, not only the fetch response headers.
// Consumers still own decoding and object URLs; this module owns one network
// read and gives every consumer an independent lightweight response view.
(() => {
    const pending = new Map();
    const stats = {shared: 0, started: 0};
    const keyFor = (url, options = {}) => {
        const headers = Object.entries(options.headers || {})
            .filter(([name]) => name.toLowerCase() !== 'x-priority')
            .sort().map(([name, value]) => `${name}:${value}`).join('|');
        return `${options.method || 'GET'} ${url} ${headers}`;
    };
    const abortable = (promise, signal) => {
        if (!signal) return promise;
        if (signal.aborted) return Promise.reject(new DOMException('Aborted', 'AbortError'));
        return Promise.race([promise, new Promise((_, reject) => signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), {once: true}))]);
    };
    const responseView = data => ({ok: data.ok, status: data.status, headers: data.headers,
        blob: () => Promise.resolve(data.blob), clone: () => responseView(data)});
    function release(entry) {
        entry.consumers--;
        if (entry.consumers <= 0 && !entry.done) {
            entry.controller.abort();
            if (pending.get(entry.key) === entry) pending.delete(entry.key);
        }
    }
    async function read(url, options = {}) {
        const key = keyFor(url, options);
        let entry = pending.get(key);
        if (!entry) {
            stats.started++;
            const controller = new AbortController();
            const requestOptions = {...options, signal: controller.signal};
            entry = {key, controller, consumers: 0, done: false};
            entry.promise = fetch(url, requestOptions).then(async response => ({ok: response.ok, status: response.status, headers: response.headers, blob: await response.blob()}));
            pending.set(key, entry);
            entry.promise.finally(() => { entry.done = true; if (pending.get(key) === entry) pending.delete(key); }).catch(() => {});
        } else stats.shared++;
        entry.consumers++;
        try { return responseView(await abortable(entry.promise, options.signal)); }
        finally { release(entry); }
    }
    function clear() { for (const entry of pending.values()) entry.controller.abort(); pending.clear(); }
    globalThis.imageReadCoordinator = {fetch: read, clear, stats, pending};
    if (typeof module !== 'undefined' && module.exports) module.exports = globalThis.imageReadCoordinator;
    if (typeof document !== 'undefined') document.addEventListener('cloudarchive:before-navigate', clear);
    if (typeof window !== 'undefined') window.addEventListener('pagehide', clear);
})();
