// Share in-flight image reads between preview, gallery and thumbnail users.
// Each caller still owns its decoded image/object URL and cache policy.
(() => {
    const pending = new Map();
    const stats = {shared: 0, started: 0};
    const keyFor = (url, options = {}) => `${options.method || 'GET'} ${url} ${Object.entries(options.headers || {}).sort().join('|')}`;
    const abortable = (promise, signal) => {
        if (!signal) return promise;
        if (signal.aborted) return Promise.reject(new DOMException('Aborted', 'AbortError'));
        return Promise.race([promise, new Promise((_, reject) => signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), {once: true}))]);
    };
    async function read(url, options = {}) {
        const key = keyFor(url, options);
        let entry = pending.get(key);
        if (!entry) {
            stats.started++;
            const controller = new AbortController();
            const requestOptions = {...options, signal: controller.signal}; delete requestOptions.priority;
            entry = {controller, promise: fetch(url, requestOptions)};
            pending.set(key, entry);
            entry.promise.finally(() => { if (pending.get(key) === entry) pending.delete(key); }).catch(() => {});
        } else stats.shared++;
        return (await abortable(entry.promise, options.signal)).clone();
    }
    function clear() { for (const entry of pending.values()) entry.controller.abort(); pending.clear(); }
    globalThis.imageReadCoordinator = {fetch: read, clear, stats, pending};
    if (typeof module !== 'undefined' && module.exports) module.exports = globalThis.imageReadCoordinator;
    if (typeof document !== 'undefined') document.addEventListener('cloudarchive:before-navigate', clear);
    if (typeof window !== 'undefined') window.addEventListener('pagehide', clear);
})();
