// Priority scheduler shared by preview, gallery and thumbnail consumers.
// A URL has one task; callers receive independent response views of its body.
(() => {
    const pending = new Map();
    const queue = [];
    const MAX_ACTIVE = 9;
    let active = 0, sequence = 0;
    const stats = {shared: 0, started: 0, queued: 0, foreground: 0};
    const keyFor = (url, options = {}) => {
        const headers = Object.entries(options.headers || {})
            .filter(([name]) => name.toLowerCase() !== 'x-priority')
            .sort().map(([name, value]) => `${name}:${value}`).join('|');
        return `${options.method || 'GET'} ${url} ${headers}`;
    };
    const responseView = data => ({ok: data.ok, status: data.status, headers: data.headers,
        blob: () => Promise.resolve(data.blob), clone: () => responseView(data)});
    const abortError = () => new DOMException('Aborted', 'AbortError');
    function pump() {
        queue.sort((a, b) => b.priority - a.priority || a.sequence - b.sequence);
        while (active < MAX_ACTIVE && queue.length) {
            const entry = queue.shift();
            if (entry.cancelled || pending.get(entry.key) !== entry) continue;
            start(entry);
        }
    }
    function start(entry) {
        active++; stats.started++; entry.state = 'active';
        const controller = new AbortController(); entry.controller = controller;
        const requestOptions = {...entry.options, signal: controller.signal};
        fetch(entry.url, requestOptions).then(async response => ({
            ok: response.ok, status: response.status, headers: response.headers,
            blob: await response.blob(),
        })).then(entry.resolve, entry.reject).finally(() => {
            active--; entry.done = true;
            if (pending.get(entry.key) === entry) pending.delete(entry.key);
            pump();
        });
    }
    function abortIfUnused(entry) {
        if (entry.consumers > 0 || entry.done) return;
        entry.cancelled = true;
        if (entry.state === 'active') entry.controller?.abort();
        if (pending.get(entry.key) === entry) pending.delete(entry.key);
        entry.reject(abortError());
        pump();
    }
    function abortable(promise, signal) {
        if (!signal) return promise;
        if (signal.aborted) return Promise.reject(abortError());
        return Promise.race([promise, new Promise((_, reject) => signal.addEventListener('abort', () => reject(abortError()), {once: true}))]);
    }
    async function read(url, options = {}) {
        const key = keyFor(url, options);
        let entry = pending.get(key);
        const priority = options.priority === 'low' ? 0 : 1;
        if (!entry) {
            let resolve, reject;
            const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
            entry = {key, url, options: {...options}, resolve, reject, promise, consumers: 0,
                priority, sequence: sequence++, state: 'queued', done: false, cancelled: false};
            delete entry.options.priority;
            pending.set(key, entry); queue.push(entry); stats.queued++; if (priority) stats.foreground++;
            pump();
        } else {
            stats.shared++;
            if (priority > entry.priority && entry.state === 'queued') entry.priority = priority;
        }
        entry.consumers++;
        try { return responseView(await abortable(entry.promise, options.signal)); }
        finally { entry.consumers--; abortIfUnused(entry); }
    }
    function clear() {
        for (const entry of [...pending.values()]) { entry.consumers = 0; abortIfUnused(entry); }
        queue.length = 0;
    }
    const api = {fetch: read, clear, stats, pending, get active() { return active; }, get queued() { return queue.length; }};
    globalThis.imageReadCoordinator = api;
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof document !== 'undefined') document.addEventListener('cloudarchive:before-navigate', clear);
    if (typeof window !== 'undefined') window.addEventListener('pagehide', clear);
})();
