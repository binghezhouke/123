/* Same-folder gallery: cap cached Blob bytes, not decoded pixels or total browser memory. */
(() => {
    const buttons = Array.from(document.querySelectorAll('.zip-image'));
    const dialog = document.getElementById('zip-gallery');
    if (!dialog || !buttons.length) return;
    const el = name => document.getElementById(`zip-gallery-${name}`);
    const cache = new Map();
    let cacheBytes = 0;
    const MAX_CACHE_IMAGES = 30;
    const MAX_CACHE_BYTES = 256 * 1024 * 1024;
    let selected = 0;
    let direction = 1;
    let generation = 0;
    const pending = new Map();
    let touchStart;
    const PREFETCH_COUNT = 9;
    const PREFETCH_CONCURRENCY = 2;
    const attempted = new Set();
    let prefetchReady = false;

    function clearCache() {
        for (const task of pending.values()) task.controller.abort();
        pending.clear();
        for (const {url} of cache.values()) URL.revokeObjectURL(url);
        cache.clear();
        cacheBytes = 0;
    }
    function prune() {
        for (const [index, task] of pending) {
            const distance = (index - selected) * direction;
            if (distance < -1 || distance > PREFETCH_COUNT) {
                task.controller.abort();
                pending.delete(index);
            }
        }
    }
    function remember(index, blob, url) {
        cache.set(index, {url, size: blob.size || 0});
        cacheBytes += blob.size || 0;
        while (cache.size > MAX_CACHE_IMAGES || cacheBytes > MAX_CACHE_BYTES) {
            const oldest = Array.from(cache.keys()).find(key => key !== selected);
            if (oldest === undefined) break;
            const entry = cache.get(oldest);
            URL.revokeObjectURL(entry.url);
            cacheBytes -= entry.size;
            cache.delete(oldest);
        }
    }
    function load(index) {
        if (cache.has(index)) {
            const entry = cache.get(index);
            cache.delete(index);
            cache.set(index, entry);
            return Promise.resolve(entry.url);
        }
        if (pending.has(index)) return pending.get(index).promise;
        const task = {controller: new AbortController()};
        const signal = task.controller.signal;
        task.promise = (async () => {
            const response = await fetch(buttons[index].dataset.url, {signal});
            if (response.status === 401) {
                throw new Error('解压密码已失效或不正确，请关闭预览并重新输入密码');
            }
            if (!response.ok || !response.headers.get('Content-Type')?.startsWith('image/')) {
                throw new Error(`图片读取失败（HTTP ${response.status}），请重试或下载原图`);
            }
            const blob = await response.blob();
            if (signal.aborted) throw new DOMException('Aborted', 'AbortError');
            const url = URL.createObjectURL(blob);
            remember(index, blob, url);
            return url;
        })().finally(() => {
            if (pending.get(index) === task) pending.delete(index);
        });
        pending.set(index, task);
        return task.promise;
    }
    function pumpPrefetch() {
        if (!dialog.open || !prefetchReady) return;
        for (let step = 1; step <= PREFETCH_COUNT; step++) {
            const index = selected + step * direction;
            if (index < 0 || index >= buttons.length) break;
            if (pending.size >= PREFETCH_CONCURRENCY) break;
            if (cache.has(index) || pending.has(index) || attempted.has(index)) continue;
            // Attempt each image once per selection, even when the byte budget evicts it.
            attempted.add(index);
            load(index).then(pumpPrefetch, pumpPrefetch);
        }
    }
    async function show(index) {
        if (index < 0 || index >= buttons.length) return;
        if (index !== selected) direction = Math.sign(index - selected);
        selected = index;
        prefetchReady = false;
        attempted.clear();
        const token = ++generation;
        prune();
        el('stage').replaceChildren();
        el('title').textContent = buttons[index].dataset.name;
        el('counter').textContent = `${index + 1} / ${buttons.length}`;
        el('download').href = `${buttons[index].dataset.url}${buttons[index].dataset.url.includes('?') ? '&' : '?'}download=1`;
        el('prev').disabled = index === 0;
        el('next').disabled = index === buttons.length - 1;
        el('retry').hidden = true;
        el('status').textContent = '正在读取图片…';
        try {
            const url = await load(index);
            if (token !== generation || !dialog.open) return;
            const image = new Image();
            image.alt = buttons[index].dataset.name;
            image.onload = () => {
                if (token !== generation) return;
                el('status').textContent = '← → 切换图片，也可左右滑动';
                prefetchReady = true;
                pumpPrefetch();
            };
            image.onerror = () => {
                if (token !== generation) return;
                el('status').textContent = '浏览器无法显示此图片，可下载原图查看';
                el('retry').hidden = false;
            };
            image.src = url;
            el('stage').replaceChildren(image);
        } catch (error) {
            if (token !== generation || error.name === 'AbortError') return;
            el('status').textContent = error.message;
            el('retry').hidden = false;
        }
    }
    buttons.forEach((button, index) => button.addEventListener('click', () => {
        if (!dialog.open) {
            direction = 1;
            selected = index;
        }
        dialog.showModal();
        show(index);
    }));
    el('prev').onclick = () => show(selected - 1);
    el('next').onclick = () => show(selected + 1);
    el('retry').onclick = () => {
        if (cache.has(selected)) {
            const entry = cache.get(selected);
            URL.revokeObjectURL(entry.url);
            cacheBytes -= entry.size;
        }
        cache.delete(selected);
        show(selected);
    };
    el('close').onclick = () => dialog.close();
    dialog.addEventListener('keydown', event => {
        if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault();
            show(selected + (event.key === 'ArrowLeft' ? -1 : 1));
        }
    });
    el('stage').addEventListener('touchstart', event => {
        touchStart = event.touches.length === 1 ? event.touches[0].clientX : undefined;
    }, {passive: true});
    el('stage').addEventListener('touchend', event => {
        if (touchStart === undefined) return;
        const delta = event.changedTouches[0].clientX - touchStart;
        touchStart = undefined;
        if (Math.abs(delta) > 60) show(selected + (delta < 0 ? 1 : -1));
    }, {passive: true});
    dialog.addEventListener('close', () => {
        ++generation;
        prefetchReady = false;
        attempted.clear();
        el('stage').replaceChildren();
        clearCache();
    });
})();
