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
            if (stored) { this.fallback.delete(key); return; }
            // Private mode, a full quota or unavailable IndexedDB must not
            // prevent viewing. Retain at most four encoded originals in RAM.
            this.fallback.delete(key); this.fallback.set(key, {version, blob});
            while (this.fallback.size > 4) this.fallback.delete(this.fallback.keys().next().value);
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
        constructor(store = new OriginalStore()) { this.store = store; this.pending = new Map(); }
        read(item, {signal, priority = 'foreground'} = {}) {
            if (signal?.aborted) return Promise.reject(aborted());
            const key = identity(item), revision = version(item), jobKey = JSON.stringify([key, revision]);
            let job = this.pending.get(jobKey);
            if (!job) {
                job = {controller: new AbortController(), users: new Set(), priority, done: false};
                this.pending.set(jobKey, job);
                job.promise = this.load(item, key, revision, job).finally(() => {
                    job.done = true;
                    if (this.pending.get(jobKey) === job) this.pending.delete(jobKey);
                });
            } else if (priority === 'foreground') job.priority = priority;
            const owner = {};
            job.users.add(owner);
            return new Promise((resolve, reject) => {
                const release = () => {
                    signal?.removeEventListener('abort', cancel);
                    job.users.delete(owner);
                    if (!job.done && job.users.size === 0) {
                        job.controller.abort();
                        if (this.pending.get(jobKey) === job) this.pending.delete(jobKey);
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
            const sources = direct && (job.priority !== 'foreground' || legacyDirect) ? [direct, local] : [local];
            let lastError;
            for (const source of [...new Set(sources)]) {
                try {
                    const response = await fetch(source, {signal: job.controller.signal, cache: 'force-cache', priority: job.priority === 'foreground' ? 'high' : 'low',
                        headers: job.priority === 'foreground' || source !== local ? {} : {'X-Priority': 'background'}});
                    const contentType = response.headers.get('Content-Type') || '';
                    if (!response.ok || (source === local && !contentType.startsWith('image/')) || /text\/html|application\/json/i.test(contentType)) throw new Error(`图片读取失败（HTTP ${response.status}）`);
                    const blob = await response.blob();
                    if (job.controller.signal.aborted) throw aborted();
                    await this.store.put(key, revision, blob);
                    return {blob, cached: false, timing: response.headers.get('Server-Timing') || ''};
                } catch (error) {
                    if (error.name === 'AbortError') throw error;
                    lastError = error;
                }
            }
            throw lastError || new Error('图片读取失败');
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
