// Encoded originals shared by thumbnails, the preview pane and the gallery.
// IndexedDB also works on private-network HTTP origins where CacheStorage may
// be unavailable. No decoded Image, bitmap, or object URL is retained here.
(() => {
    if (globalThis.CloudImageResources) return;
    const aborted = () => new DOMException('Aborted', 'AbortError');
    const ORIGINAL_LIMIT = 200;
    class OriginalStore {
        constructor() {
            this.fallback = new Map();
            this.db = new Promise(resolve => {
                if (!globalThis.indexedDB) return resolve(null);
                let request;
                try { request = indexedDB.open('cloudarchive-originals-v1', 1); } catch { return resolve(null); }
                request.onupgradeneeded = () => request.result.createObjectStore('images', {keyPath: 'key'}).createIndex('at', 'at');
                request.onsuccess = () => { request.result.onversionchange = () => request.result.close(); resolve(request.result); };
                request.onerror = request.onblocked = () => resolve(null);
            });
        }
        async get(key, version) {
            const fallback = this.fallback.get(key);
            if (fallback && (!version || !fallback.version || version === fallback.version)) {
                this.fallback.delete(key); this.fallback.set(key, fallback); return fallback.blob;
            }
            const db = await this.db;
            if (!db) return null;
            return new Promise(resolve => {
                try {
                    const tx = db.transaction('images', 'readwrite'), store = tx.objectStore('images');
                    let blob = null;
                    const request = store.get(key);
                    request.onsuccess = () => {
                        const row = request.result;
                        if (!row || (version && row.version && version !== row.version)) return;
                        blob = row.blob; row.at = Date.now(); store.put(row);
                    };
                    tx.oncomplete = () => resolve(blob);
                    tx.onerror = tx.onabort = () => resolve(null);
                } catch { resolve(null); }
            });
        }
        async put(key, version, blob) {
            // Hand completed bytes to subsequent displays immediately, even
            // while IndexedDB is busy. Only four encoded blobs stay in RAM.
            this.fallback.delete(key); this.fallback.set(key, {version, blob});
            while (this.fallback.size > 4) this.fallback.delete(this.fallback.keys().next().value);
            const db = await this.db;
            const stored = db && await new Promise(resolve => {
                try {
                    const tx = db.transaction('images', 'readwrite'), store = tx.objectStore('images');
                    store.put({key, version, blob, at: Date.now()});
                    const count = store.count();
                    count.onsuccess = () => {
                        let excess = count.result - ORIGINAL_LIMIT;
                        if (excess <= 0) return;
                        // Key cursors avoid loading other original blobs while
                        // evicting the least recently used encoded files.
                        const cursor = store.index('at').openKeyCursor();
                        cursor.onsuccess = () => {
                            const entry = cursor.result;
                            if (!entry || excess <= 0) return;
                            if (entry.primaryKey !== key) { store.delete(entry.primaryKey); excess--; }
                            entry.continue();
                        };
                    };
                    tx.oncomplete = () => resolve(true);
                    tx.onerror = tx.onabort = () => resolve(false);
                } catch { resolve(false); }
            });
            if (stored && this.fallback.get(key)?.blob === blob) this.fallback.delete(key);
        }
        async remove(key) {
            this.fallback.delete(key);
            const db = await this.db;
            if (!db) return;
            await new Promise(resolve => {
                try {
                    const tx = db.transaction('images', 'readwrite'); tx.objectStore('images').delete(key);
                    tx.oncomplete = tx.onerror = tx.onabort = resolve;
                } catch { resolve(); }
            });
        }
    }

    function identity(item) {
        const data = item.dataset;
        const url = new URL(data.url || data.memberId || data.favoriteUrl, location.href);
        url.hash = ''; url.searchParams.delete('direct'); url.searchParams.delete('download'); url.searchParams.sort();
        return url.href;
    }
    function version(item) {
        // Archive content is immutable, including nested members. Plain files
        // invalidate a stored original when supplied listing metadata changes.
        if (item.dataset.memberId || new URL(identity(item)).pathname.startsWith('/archive/')) return '';
        return item.dataset.updated || item.dataset.size ? JSON.stringify([item.dataset.updated || '', item.dataset.size || '']) : '';
    }
    class ImageResources {
        constructor(store = new OriginalStore()) {
            this.store = store; this.pending = new Map();
            // The local HTTP/1 origin has a small browser connection pool.
            // This is shared transfer admission, not a platform API limiter.
            this.backgroundLimit = 4; this.backgroundActive = 0;
            this.waiting = []; this.foreground = new Set();
        }
        promote(item) {
            const job = this.pending.get(JSON.stringify([identity(item), version(item)]));
            if (job) this.promoteJob(job);
        }
        promoteJob(job) {
            if (job.done || job.priority === 'foreground') return;
            job.priority = 'foreground'; this.foreground.add(job);
            // Fetch priority/headers cannot be changed after dispatch. Keep
            // the shared promise and consumers, replace only this attempt.
            if (job.attempt?.background) {
                job.attempt.promoted = true;
                job.attempt.controller.abort();
            }
            this.pump();
        }
        acquire(job, localPool = true) {
            return new Promise((resolve, reject) => {
                const entry = {job, resolve, reject, localPool};
                entry.cancel = () => {
                    this.waiting = this.waiting.filter(value => value !== entry);
                    reject(aborted());
                };
                if (job.controller.signal.aborted) return entry.cancel();
                job.controller.signal.addEventListener('abort', entry.cancel, {once: true});
                this.waiting.push(entry); this.pump();
            });
        }
        pump() {
            for (const entry of [...this.waiting]) {
                const background = entry.job.priority !== 'foreground';
                if (background && (this.foreground.size || (entry.localPool && this.backgroundActive >= this.backgroundLimit))) continue;
                this.waiting.splice(this.waiting.indexOf(entry), 1);
                entry.job.controller.signal.removeEventListener('abort', entry.cancel);
                if (background && entry.localPool) this.backgroundActive++;
                let released = false;
                entry.resolve(() => {
                    if (released) return;
                    released = true;
                    if (background && entry.localPool) this.backgroundActive--;
                    this.pump();
                });
            }
        }
        read(item, {signal, priority = 'foreground'} = {}) {
            if (signal?.aborted) return Promise.reject(aborted());
            const key = identity(item), revision = version(item), jobKey = JSON.stringify([key, revision]);
            let job = this.pending.get(jobKey);
            if (!job) {
                job = {controller: new AbortController(), users: new Set(), priority, done: false};
                this.pending.set(jobKey, job);
                if (priority === 'foreground') this.foreground.add(job);
                job.promise = this.load(item, key, revision, job).finally(() => {
                    job.done = true;
                    if (this.pending.get(jobKey) === job) this.pending.delete(jobKey);
                    this.foreground.delete(job); this.pump();
                });
            } else if (priority === 'foreground') this.promoteJob(job);
            const owner = {};
            job.users.add(owner);
            return new Promise((resolve, reject) => {
                const release = () => {
                    signal?.removeEventListener('abort', cancel);
                    job.users.delete(owner);
                    if (!job.done && job.users.size === 0) {
                        job.controller.abort();
                        if (this.pending.get(jobKey) === job) this.pending.delete(jobKey);
                        this.foreground.delete(job); this.pump();
                    }
                };
                const cancel = () => { release(); reject(aborted()); };
                signal?.addEventListener('abort', cancel, {once: true});
                job.promise.then(value => { release(); signal?.aborted ? reject(aborted()) : resolve(value); }, error => { release(); reject(error); });
            });
        }
        async load(item, key, revision, job) {
            const saved = await this.store.get(key, revision);
            if (job.controller.signal.aborted) throw aborted();
            if (saved) return {blob: saved, cached: true, timing: ''};
            const data = item.dataset, local = key;
            const legacyDirect = new URL(data.url || local, location.href).searchParams.get('direct') === '1';
            const direct = data.directUrl || (legacyDirect ? data.url : '');
            for (;;) {
                const priority = job.priority;
                const sources = direct && (priority !== 'foreground' || legacyDirect) ? [direct, local] : [local];
                let lastError, promoted = false;
                for (const source of [...new Set(sources)]) {
                    const localOrigin = new URL(local).origin;
                    const release = await this.acquire(job, new URL(source, local).origin === localOrigin);
                    if (priority !== job.priority) { release(); promoted = true; break; }
                    const attempt = {controller: new AbortController(), background: priority !== 'foreground', promoted: false};
                    job.attempt = attempt;
                    const cancel = () => attempt.controller.abort();
                    job.controller.signal.addEventListener('abort', cancel, {once: true});
                    try {
                        if (job.controller.signal.aborted) throw aborted();
                        const response = await fetch(source, {signal: attempt.controller.signal, cache: 'force-cache', priority: attempt.background ? 'low' : 'high',
                            headers: !attempt.background || source !== local ? {} : {'X-Priority': 'background'}});
                        // A redirect has released the application's HTTP
                        // connection. CDN body transfers use their own pool.
                        if (response.url && new URL(response.url, local).origin !== localOrigin) release();
                        const contentType = response.headers.get('Content-Type') || '';
                        if (!response.ok || (source === local && !contentType.startsWith('image/')) || /text\/html|application\/json/i.test(contentType)) {
                            await response.body?.cancel();
                            throw new Error(`图片读取失败（HTTP ${response.status}）`);
                        }
                        const blob = await response.blob();
                        if (job.controller.signal.aborted || attempt.controller.signal.aborted) throw aborted();
                        // Persistence is useful for the next visit, not a
                        // prerequisite for displaying bytes already received.
                        this.store.put(key, revision, blob).catch(() => {});
                        return {blob, cached: false, timing: response.headers.get('Server-Timing') || ''};
                    } catch (error) {
                        if (attempt.promoted && !job.controller.signal.aborted) { promoted = true; break; }
                        if (error.name === 'AbortError') throw error;
                        lastError = error;
                    } finally {
                        job.controller.signal.removeEventListener('abort', cancel);
                        if (job.attempt === attempt) job.attempt = null;
                        release();
                    }
                }
                if (!promoted) throw lastError || new Error('图片读取失败');
            }
        }
        forget(item) { return this.store.remove(identity(item)); }
        // A display owns only its object URL. Replacing the preview never
        // invalidates the encoded original or another consumer's request.
        display(image, item) {
            const controller = new AbortController();
            let url;
            this.read(item, {signal: controller.signal}).then(result => {
                if (controller.signal.aborted) return;
                url = URL.createObjectURL(result.blob); image.src = url;
            }).catch(error => { if (error.name !== 'AbortError') image.alt = `图片加载失败：${error.message}`; });
            return () => { controller.abort(); image.onload = null; image.onerror = null; image.removeAttribute('src'); if (url) URL.revokeObjectURL(url); };
        }
    }
    globalThis.CloudImageResources = new ImageResources();
    if (typeof module !== 'undefined' && module.exports) module.exports = {ImageResources, OriginalStore, identity, version};
})();
