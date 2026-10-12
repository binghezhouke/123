// Encoded thumbnails live in IndexedDB; only near-viewport images get object URLs.
(() => {
    class ThumbnailStore {
        constructor() {
            this.db = new Promise(resolve => {
                if (!globalThis.indexedDB) return resolve(null);
                const request = indexedDB.open('cloudarchive-thumbnails-v1', 2);
                request.onupgradeneeded = () => {
                    const db = request.result;
                    const store = db.objectStoreNames.contains('images') ? request.transaction.objectStore('images') : db.createObjectStore('images', {keyPath: 'key'});
                    if (!store.indexNames.contains('at')) store.createIndex('at', 'at');
                };
                request.onsuccess = () => resolve(request.result);
                request.onerror = request.onblocked = () => resolve(null);
            });
        }
        async get(key) {
            const db = await this.db;
            if (!db) return null;
            return new Promise(resolve => {
                try {
                    const tx = db.transaction('images', 'readwrite'), store = tx.objectStore('images');
                    const request = store.get(key);
                    request.onsuccess = () => {
                        const row = request.result;
                        if (!row) return resolve(null);
                        row.at = Date.now();
                        store.put(row);
                        resolve(row.blob || null);
                    };
                    request.onerror = () => resolve(null);
                } catch { resolve(null); }
            });
        }
        async put(key, blob) {
            const db = await this.db;
            if (!db) return;
            await new Promise(resolve => {
                try {
                    const tx = db.transaction('images', 'readwrite'), store = tx.objectStore('images');
                    store.put({key, blob, at: Date.now()});
                    const request = store.getAll();
                    request.onsuccess = () => {
                        const rows = request.result.sort((a,b) => b.at-a.at);
                        let bytes = 0;
                        rows.forEach((row, index) => {
                            bytes += row.blob.size;
                            if (index >= 2000 || bytes > 128 * 1024 * 1024) store.delete(row.key);
                        });
                    };
                    tx.oncomplete = tx.onerror = tx.onabort = () => resolve();
                } catch { resolve(); }
            });
        }
    }
    async function thumbnail(blob) {
        // Width-only resizing preserves the aspect ratio. Release the bitmap
        // after encoding; never retain the original Image element in a cache.
        if (typeof createImageBitmap === 'undefined') {
            // Older engines do not expose createImageBitmap. Keep the same
            // residency rule by drawing through a short-lived Image element;
            // the original blob is never retained after this function returns.
            if (typeof Image === 'undefined') return blob;
            const source = URL.createObjectURL(blob);
            try {
                const image = await new Promise((resolve, reject) => {
                    const value = new Image();
                    value.onload = () => resolve(value);
                    value.onerror = () => reject(Error('图片解码失败'));
                    value.src = source;
                });
                const canvas = document.createElement('canvas');
                const scale = Math.min(1, 180 / image.width, 180 / image.height);
                canvas.width = Math.max(1, Math.round(image.width * scale));
                canvas.height = Math.max(1, Math.round(image.height * scale));
                canvas.getContext('2d').drawImage(image, 0, 0, canvas.width, canvas.height);
                return await new Promise((resolve, reject) => canvas.toBlob(b => b ? resolve(b) : reject(Error('缩略图编码失败')), 'image/jpeg', .78));
            } finally { URL.revokeObjectURL(source); }
        }
        const bitmap = await createImageBitmap(blob, {resizeWidth: 180, resizeQuality: 'medium'});
        try {
            const canvas = document.createElement('canvas');
            const scale = Math.min(1, 180 / bitmap.height);
            canvas.width = Math.max(1, Math.round(bitmap.width * scale));
            canvas.height = Math.max(1, Math.round(bitmap.height * scale));
            canvas.getContext('2d').drawImage(bitmap, 0, 0, canvas.width, canvas.height);
            return await new Promise((resolve, reject) => canvas.toBlob(b => b ? resolve(b) : reject(Error('缩略图编码失败')), 'image/jpeg', .78));
        } finally { bitmap.close(); }
    }
    class ViewportImages {
        constructor(root, output) {
            this.root = root; this.output = output; this.store = new ThumbnailStore();
            this.near = new Set(); this.urls = new Map(); this.jobs = new Map(); this.queue = [];
            this.active = 0; this.hits = 0; this.observed = new WeakSet(); this.sequence = 0; this.closed = false;
            this.observer = new IntersectionObserver(entries => {
                for (const entry of entries) {
                    if (entry.isIntersecting) { this.near.add(entry.target); this.enqueue(entry.target); }
                    else { this.near.delete(entry.target); this.release(entry.target); }
                }
                this.pump(); this.render();
            }, {root, rootMargin: `${Math.round(root.clientHeight / 2)}px 0px`});
            this.observe();
            document.addEventListener('files:appended', () => this.observe());
            document.addEventListener('cloudarchive:navigated', () => this.reset());
            window.addEventListener('pagehide', () => this.close());
        }
        key(item) {
            // Archive members do not have a cloud file ID; using the old
            // undefined-based key made every same-sized member share one
            // thumbnail. Prefer the stable member URL/path before metadata.
            const identity = item.dataset.fileId || item.dataset.memberId || item.dataset.url || item.dataset.favoriteUrl || item.dataset.name || 'unknown';
            return [identity, item.dataset.updated, item.dataset.size].join(':');
        }
        observe() {
            document.querySelectorAll('.file-item[data-gallery-image]').forEach(item => {
                if (!this.observed.has(item)) { this.observed.add(item); this.observer.observe(item); }
            });
        }
        enqueue(item) {
            if (this.closed || this.urls.has(item) || this.jobs.has(item)) return;
            const job = {state: '排队', sequence: this.sequence++, controller: new AbortController()};
            this.jobs.set(item, job); this.queue.push({item, job});
        }
        distance(item) {
            if (!this.root?.getBoundingClientRect) return Number.POSITIVE_INFINITY;
            const root = this.root.getBoundingClientRect(), rect = item.getBoundingClientRect?.();
            if (!rect || rect.width === 0 || rect.height === 0) return Number.POSITIVE_INFINITY;
            if (rect.bottom < root.top) return root.top - rect.bottom;
            if (rect.top > root.bottom) return rect.top - root.bottom;
            return 0;
        }
        pump() {
            // IntersectionObserver does not guarantee entry order. Always
            // decode visible rows first, then the nearest rows above/below;
            // this prevents a far prefetch from occupying both slots while
            // the image the user can see is still waiting.
            this.queue.sort((a, b) => this.distance(a.item) - this.distance(b.item) || a.job.sequence - b.job.sequence);
            while (!this.closed && this.active < 2 && this.queue.length) {
                const entry = this.queue.shift(), item = entry.item, job = entry.job;
                if (!this.near.has(item) || this.jobs.get(item) !== job) { continue; }
                this.active++;
                this.load(item, job).catch(error => {
                    if (error.name !== 'AbortError' && this.near.has(item) && this.jobs.get(item) === job) job.state = `失败：${error.message}`;
                })
                    .finally(() => { this.active--; this.pump(); this.render(); });
            }
        }
        async load(item, job) {
            const key = this.key(item);
            if (this.jobs.get(item) !== job) return;
            job.state = '读取本地缩略图'; this.render();
            let blob = await this.store.get(key);
            if (blob) this.hits++;
            else {
                if (!this.near.has(item) || this.closed || job.controller.signal.aborted) return;
                job.state = '下载原图 / 生成缩略图'; this.render();
                // Prefer the platform direct URL so this fetch can reuse the
                // browser's URL cache without another API call. Fall back to
                // the same-origin proxy when direct access is unavailable.
                const sources = [item.dataset.directUrl, item.dataset.url].filter(Boolean);
                let response, lastError;
                for (const source of sources) {
                    try {
                        response = await fetch(source, {cache:'force-cache', priority:'low', signal:job.controller.signal});
                        if (!response.ok) throw Error(`HTTP ${response.status}`);
                        break;
                    } catch (error) {
                        if (error.name === 'AbortError') throw error;
                        lastError = error;
                        response = null;
                    }
                }
                if (!response) throw (lastError || Error('图片读取失败'));
                const original = await response.blob();
                blob = await thumbnail(original);
                await this.store.put(key, blob);
            }
            if (this.closed || !this.near.has(item) || job.controller.signal.aborted || (this.jobs.get(item) && this.jobs.get(item) !== job)) return;
            if (this.jobs.get(item) === job) this.jobs.delete(item);
            const icon = item.querySelector('.file-grid-icon, .file-list-icon');
            if (!icon) return;
            const url = URL.createObjectURL(blob), img = document.createElement('img');
            img.className = 'preview-prefetch-thumb'; img.alt = ''; img.src = url;
            icon.querySelector('i')?.classList.add('preview-prefetch-icon-hidden'); icon.append(img);
            this.urls.set(item, url);
        }
        release(item) {
            const url = this.urls.get(item);
            item.querySelector('.preview-prefetch-thumb')?.remove();
            item.querySelector('i')?.classList.remove('preview-prefetch-icon-hidden');
            if (url) URL.revokeObjectURL(url);
            this.urls.delete(item);
            const job = this.jobs.get(item);
            if (job) {
                job.controller.abort();
                this.jobs.delete(item);
            }
        }
        render() {
            if (!this.output) return;
            const lines = [`视口附近 ${this.near.size} · 驻留缩略图 ${this.urls.size} · 工作 ${this.active}/2 · 本地命中 ${this.hits}`];
            for (const item of this.near) lines.push(`${this.urls.has(item) ? '缩略图驻留' : this.jobs.get(item)?.state || '等待'} · ${item.dataset.favoriteName}`);
            this.output.textContent = lines.join('\n');
        }
        reset() {
            if (this.closed) return;
            this.observer.disconnect(); this.queue = [];
            for (const item of [...this.jobs.keys()]) this.release(item);
            for (const item of [...this.urls.keys()]) this.release(item);
            this.near.clear(); this.observed = new WeakSet(); this.observe(); this.render();
        }
        close() { this.closed = true; this.observer.disconnect(); this.queue = []; for (const item of [...this.jobs.keys()]) this.release(item); for (const item of [...this.urls.keys()]) this.release(item); }
    }
    globalThis.CloudViewportImages = ViewportImages;
})();
