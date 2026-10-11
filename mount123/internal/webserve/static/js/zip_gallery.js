/* Shared directory/archive image viewer with bounded blob and thumbnail caches. */
(() => {
    const dialog = document.getElementById('zip-gallery');
    if (!dialog) return;
    const el = name => document.getElementById(`zip-gallery-${name}`);
    const viewport = typeof createArchiveImageViewport === 'function' ? createArchiveImageViewport(dialog) : null;
    const cache = new Map();
    const pending = new Map();
    const attempted = new Set();
    const stats = {hits: 0, misses: 0, failures: 0, durations: [], extracts: [], cacheBytes: 0};
    // Download requests use cached short-lived URLs and are separate from
    // platform API calls. Keep a generous client-side window for direct-link
    // reads while the API scheduler continues to protect metadata requests.
    const DEFAULTS = {count: 9, concurrency: 9, budget: 30};
    const SETTINGS_KEY = 'imageGallerySettings';
    let settings = readSettings();
    let selected = 0, direction = 1, generation = 0, prefetchReady = false, touchStart;
    let exifVisible = false;
    let thumbController = null, thumbQueue = [], indexController = null, indexPromise = null, indexComplete = false, indexGeneration = 0;

    function readSettings() {
        try {
            const value = JSON.parse(localStorage.getItem(SETTINGS_KEY) || '{}');
            const legacyDefault = Number(value.concurrency) === 6 && Number(value.count) === 9 && Number(value.budget) === 256;
            const concurrency = legacyDefault ? DEFAULTS.concurrency : clamp(value.concurrency, 1, 12, DEFAULTS.concurrency);
            return {count: clamp(value.count, 0, 50, DEFAULTS.count), concurrency, budget: legacyDefault ? DEFAULTS.budget : clamp(value.budget, 1, 200, DEFAULTS.budget)};
        } catch { return {...DEFAULTS}; }
    }
    function clamp(value, min, max, fallback) { const n = Number(value); return Number.isFinite(n) ? Math.min(max, Math.max(min, Math.round(n))) : fallback; }
    function items() {
        const index = el('index');
        if (index?.childElementCount) return [...index.children];
        return [...document.querySelectorAll('[data-gallery-image], .zip-image')].filter(node => {
        if (node.isConnected === false) return false;
        const view = node.closest?.('#fileContainer, #fileListView');
        return !view || getComputedStyle(view).display !== 'none';
        });
    }
    function source(item) { return item.dataset.url || ''; }
    function directSource(item) {
        if (item.dataset.directUrl) return item.dataset.directUrl;
        try { return new URL(source(item), location.href).searchParams.get('direct') === '1' ? source(item) : ''; } catch { return ''; }
    }
    function proxySource(url) { try { const target = new URL(url, location.href); target.searchParams.delete('direct'); return target.pathname + target.search; } catch { return url; } }
    function preloadImage(url, signal) {
        return new Promise((resolve, reject) => {
            const image = new Image();
            const abort = () => { image.src = ''; reject(new DOMException('Aborted', 'AbortError')); };
            image.onload = () => { signal?.removeEventListener('abort', abort); resolve(image); };
            image.onerror = () => { signal?.removeEventListener('abort', abort); reject(new Error('图片读取失败')); };
            if (signal?.aborted) return abort();
            signal?.addEventListener('abort', abort, {once:true});
            image.src = url;
        });
    }
    function identity(item) { return item.dataset.fileId || item.dataset.memberId || source(item); }
    function backgroundTasks() { return [...pending.values()].filter(task => !task.priority && !task.thumbOnly); }
    function title(item) { return item.dataset.name || item.getAttribute('aria-label') || item.textContent.trim(); }
    function renderExif(info, loading = false) {
        const panel = el('exif-panel'); if (!panel) return;
        panel.hidden = !exifVisible;
        if (!exifVisible) return;
        if (loading) { panel.innerHTML = '<strong>EXIF</strong>正在读取…'; return; }
        if (!info) { panel.innerHTML = '<strong>EXIF</strong>没有可用的 EXIF 信息'; return; }
        const escape = value => String(value).replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
        panel.innerHTML = `<strong>EXIF</strong>${Object.entries(info).map(([key, value]) => `<div>${escape(key)}：${escape(value)}</div>`).join('')}`;
    }
    function setCounter(text) { el('counter').textContent = text; const fsCounter = document.getElementById('zip-gallery-fs-counter'); if (fsCounter) fsCounter.textContent = text; }
    function pageKey() { return dialog.dataset.galleryScope || location.pathname + location.search.replace(/([?&])(last_file_id|limit)=.*?(&|$)/g, '$1'); }
    function savedProgress() { try { return JSON.parse(localStorage.getItem(`galleryProgress:${pageKey()}`) || 'null'); } catch { return null; } }
    function saveProgress(item) { try { localStorage.setItem(`galleryProgress:${pageKey()}`, JSON.stringify({identity: identity(item), name: title(item)})); const button=document.querySelector('[data-gallery-resume]'); if(button) button.hidden=false; } catch {} }
    function findIndex(list, id) { return list.findIndex(item => identity(item) === id); }
    async function waitForPage() {
        return new Promise(resolve => {
            const done = event => { document.removeEventListener('files:load-complete', done); resolve(event.detail?.ok); };
            document.addEventListener('files:load-complete', done, {once:true});
            const more = document.querySelector('[data-load-more][href]'); if (!more) { document.removeEventListener('files:load-complete', done); resolve(false); return; }
            more.click();
            setTimeout(() => { document.removeEventListener('files:load-complete', done); resolve(false); }, 20000);
        });
    }
    async function populateDirectoryIndex(activeId) {
        if (!dialog.dataset.galleryIndexUrl) return;
        if (indexComplete) return findIndex(items(), String(activeId));
        if (!indexPromise) {
            const container = el('index'), controller = new AbortController(), loadGeneration = ++indexGeneration;
            container.replaceChildren(); indexController = controller;
            const promise = (async () => {
                let next = dialog.dataset.galleryIndexUrl, pages = 0;
                while (next && pages++ < 1000) {
                    const response = await fetch(next, {headers: {'X-Requested-With':'XMLHttpRequest'}, signal:controller.signal});
                    if (!response.ok) throw new Error(`目录图片列表读取失败（HTTP ${response.status}）`);
                    const page = await response.json(); if (controller.signal.aborted || loadGeneration !== indexGeneration || !dialog.open) throw new DOMException('Aborted','AbortError');
                    for (const entry of (page.items || [])) {
                        const button = document.createElement('button'); button.type='button'; button.dataset.galleryImage=''; button.dataset.fileId=String(entry.id); button.dataset.url=entry.url; button.dataset.name=entry.name; container.append(button);
                    }
                    next = page.next;
                }
                if (pages >= 1000 && next) throw new Error('目录图片数量超出可浏览范围');
            })();
            indexPromise = promise;
            try { await promise; if (loadGeneration !== indexGeneration || !dialog.open) throw new DOMException('Aborted','AbortError'); indexComplete=true; }
            catch (error) { if (loadGeneration === indexGeneration) container.replaceChildren(); throw error; }
            finally { if (indexPromise === promise) { indexPromise=null; indexController=null; } }
        } else {
            await indexPromise;
        }
        return findIndex(items(), String(activeId));
    }
    function touchCache(index, url, size, image = null, exif = null) {
        cache.set(index, {url, size, image, exif}); stats.cacheBytes += size;
        trimCache();
    }
    function trimCache() {
        const cacheLimit = Math.max(1, settings.budget);
        const protectedKeys = new Set([selected]);
        for (let step = 1; step <= settings.count; step++) {
            const index = selected + step * direction;
            if (index < 0 || index >= items().length) break;
            protectedKeys.add(index);
        }
        while (cache.size > cacheLimit) {
            const candidates = [...cache.keys()].filter(key => !protectedKeys.has(key));
            if (!candidates.length) break;
            const victim = candidates.reduce((best, key) => {
                const bestAhead = direction * (best - selected);
                const keyAhead = direction * (key - selected);
                const bestScore = (bestAhead < 0 ? 1000000 : 0) + Math.abs(bestAhead);
                const keyScore = (keyAhead < 0 ? 1000000 : 0) + Math.abs(keyAhead);
                return keyScore > bestScore ? key : best;
            }, candidates[0]);
            const key = victim;
            if (key === undefined) break;
            const old = cache.get(key); URL.revokeObjectURL(old.url); stats.cacheBytes -= old.size; cache.delete(key);
            // An evicted item must be eligible for a future directional
            // prefetch. `attempted` only suppresses duplicate work while an
            // item is still resident (or has a current failure).
            attempted.delete(key);
        }
    }
    function cancelOutside() {
        for (const [idx, task] of pending) {
            if (Math.abs(idx - selected) > settings.count) { task.controller.abort(); pending.delete(idx); attempted.delete(idx); }
        }
    }
    function enforceConcurrency() {
        while (backgroundTasks().length > settings.concurrency) {
            const expendable = [...pending.entries()].reverse().find(([key, task]) => key !== selected && !task.priority);
            if (!expendable) break;
            expendable[1].controller.abort(); pending.delete(expendable[0]); attempted.delete(expendable[0]);
        }
    }
    function load(index, priority = false, ownerSignal = null) {
        const list = items(), item = list[index];
        if (!item) return Promise.reject(new Error('图片不存在'));
        if (cache.has(index)) { stats.hits++; const value = cache.get(index); cache.delete(index); cache.set(index, value); return Promise.resolve(value.url); }
        if (pending.has(index)) { const task=pending.get(index); if (priority) { task.thumbOnly=false; task.priority=true; } return task.promise; }
        const task = {controller: new AbortController(), priority, thumbOnly:Boolean(ownerSignal), failed:false};
        if (ownerSignal) ownerSignal.addEventListener('abort', () => { if (task.thumbOnly && pending.get(index) === task) { task.controller.abort(); pending.delete(index); } }, {once:true});
        task.promise = (async () => {
        const started = typeof performance !== 'undefined' ? performance.now() : Date.now(); stats.misses++;
            try {
                const directUrl = directSource(item);
                // New listings carry separate local/direct URLs. Keep legacy
                // pages whose only URL already has ?direct=1 working as
                // direct-first until they are refreshed.
                const legacyDirect = Boolean(directUrl && !item.dataset.directUrl);
                const localUrl = item.dataset.directUrl ? source(item) : (legacyDirect ? proxySource(source(item)) : source(item));
                const useDirect = Boolean(directUrl && (!priority || legacyDirect));
                const headers = priority ? {} : {'X-Priority': 'background'};
                let response, responseWasDirect = false;
                if (useDirect) {
                    try {
                        response = await fetch(directUrl, {signal: task.controller.signal, cache: 'force-cache'});
                        if (!response.ok) throw new Error('直链读取失败');
                        responseWasDirect = true;
                    } catch (error) {
                        if (error.name === 'AbortError') throw error;
                        response = await fetch(localUrl, {signal: task.controller.signal, headers, cache: 'force-cache'});
                    }
                } else {
                    response = await fetch(localUrl, {signal: task.controller.signal, headers, cache: 'force-cache'});
                }
                if (response.status === 401) throw new Error('解压密码已失效或不正确，请关闭预览并重新输入密码');
                const contentType = response.headers.get('Content-Type') || '';
                // CDN direct links sometimes use application/octet-stream or
                // omit Content-Type. The browser can still decode the blob;
                // only reject a non-image response from our archive proxy,
                // where an HTML/error body must not enter the image cache.
                if (!response.ok || (!responseWasDirect && !contentType.startsWith('image/'))) throw new Error(`图片读取失败（HTTP ${response.status}），请重试或下载原图`);
                const timing = response.headers.get('Server-Timing') || '';
                const extract = timing.match(/(?:^|,)\s*extract;dur=([\d.]+)/i);
                if (extract) { stats.extracts.push(Number(extract[1])); if (stats.extracts.length > 100) stats.extracts.shift(); }
                const blob = await response.blob();
                if (task.controller.signal.aborted) throw new DOMException('Aborted', 'AbortError');
                const url = URL.createObjectURL(blob);
                const exif = typeof readImageExif === 'function' ? await readImageExif(blob).catch(() => null) : null;
                touchCache(index, url, blob.size || 0, null, exif);
                stats.durations.push((typeof performance !== 'undefined' ? performance.now() : Date.now()) - started); if (stats.durations.length > 100) stats.durations.shift();
                return url;
            } catch (error) { if (error.name !== 'AbortError') { stats.failures++; task.failed=true; if (!task.priority) attempted.delete(index); } throw error; }
        })().finally(() => { if (pending.get(index) === task) pending.delete(index); renderDiagnostics(); renderDebugOverlay(); if (!task.failed) pumpPrefetch(); pumpThumbQueue(); });
        pending.set(index, task); return task.promise;
    }
    function pumpPrefetch() {
        if (!dialog.open) return;
        // Register the selected image before planning speculative work. A
        // foreground request must get the first decoder/cache slot; otherwise
        // opening a gallery can put the first image behind several neighbors.
        const selectedTask = pending.get(selected);
        if (selectedTask?.priority) return;
        const max = settings.concurrency;
        const indexes = [];
        for (let step = 1; step <= settings.count && indexes.length < settings.count; step++) {
            const index = selected + step * direction;
            if (index >= 0 && index < items().length) indexes.push(index);
        }
        for (const index of indexes) {
            if (backgroundTasks().length >= max) break;
            if (cache.has(index) || pending.has(index) || attempted.has(index)) continue;
            attempted.add(index);
            load(index).catch(() => {});
        }
    }
    function renderThumbs() {
        const list = items(), rail = el('thumbs');
        if (!rail || rail.hidden !== false || !dialog.open || !document.createElement) return;
        thumbController?.abort(); thumbController = new AbortController();
        const start = Math.max(0, selected - 3), end = Math.min(list.length, selected + 4);
        rail.replaceChildren();
        for (let i = start; i < end; i++) {
            const button = document.createElement('button'); button.type = 'button'; button.className = 'gallery-thumb';
            button.setAttribute('aria-label', `第 ${i + 1} 张：${title(list[i])}`); button.setAttribute('aria-current', String(i === selected)); button.dataset.index = String(i);
            const image = document.createElement('img'); image.alt = ''; image.loading = 'lazy'; button.append(image);
            button.onclick = () => show(Number(button.dataset.index)); rail.append(button);
            thumbQueue.push({i, button, image, controller:thumbController});
        }
        pumpThumbQueue();
        if (el('jump-input')) { el('jump-input').max = String(list.length); el('jump-input').value = String(selected + 1); }
    }
    function pumpThumbQueue() {
        if (!dialog.open || backgroundTasks().length >= settings.concurrency || !thumbQueue.length) return;
        const entry=thumbQueue.shift();
        if (entry.controller.signal.aborted || !entry.button.isConnected) { pumpThumbQueue(); return; }
        load(entry.i, false, entry.controller.signal).then(url => { if (!entry.controller.signal.aborted && entry.button.isConnected) entry.image.src=url; }).catch(error => { if(error.name!=='AbortError' && entry.button.isConnected) entry.button.classList.add('thumb-failed'); }).finally(pumpThumbQueue);
    }
    async function show(index) {
        let list = items();
        if (index < 0) return;
        if (index >= list.length) {
            const more = document.querySelector('[data-load-more][href]');
            if (!more) return;
            const oldCount = list.length; more.click();
            el('status').textContent = '正在加载下一页…';
            for (let i = 0; i < 100 && items().length === oldCount; i++) await new Promise(resolve => setTimeout(resolve, 100));
            list = items(); if (index >= list.length) { el('status').textContent = '下一页加载失败，请先在目录中重试加载'; return; }
        }
        if (index !== selected) { direction = Math.sign(index - selected) || direction; attempted.clear(); }
        selected = index; prefetchReady = false; const token = ++generation; cancelOutside(); viewport?.detach();
        const panel = el('exif-panel'); el('stage').replaceChildren(); if (panel) el('stage').append(panel);
        renderExif(null, true); el('title').textContent = title(list[index]); setCounter(`${index + 1} / ${list.length}`);
        const url = source(list[index]); el('download').href = `${url}${url.includes('?') ? '&' : '?'}download=1`;
        el('prev').disabled = index === 0; el('next').disabled = index === list.length - 1 && !document.querySelector?.('[data-load-more][href]'); el('retry').hidden = true; el('status').textContent = '正在读取图片…'; saveProgress(list[index]);
        try {
            // Start the foreground request before allowing pumpPrefetch() to
            // schedule neighbors. load() installs the priority task
            // synchronously, so the guard above keeps the first image ahead.
            const foreground = load(index, true);
            pumpPrefetch();
            const objectUrl = await foreground; if (token !== generation || !dialog.open) return;
            renderExif(cache.get(index)?.exif || null);
            const retained = cache.get(index)?.image;
            const image = retained || new Image(); image.alt = title(list[index]); image.onload = () => { if (token !== generation) return; viewport?.attach(image); el('status').textContent = '滚轮或双指缩放 · 放大后拖动 · ← → 切换图片'; if (image.naturalWidth && image.naturalHeight) { setCounter(`${index + 1} / ${list.length} · ${image.naturalWidth} × ${image.naturalHeight}`); } prefetchReady = true; renderThumbs(); pumpPrefetch(); };
            image.onerror = () => { if (token !== generation) return; el('status').textContent = '浏览器无法显示此图片，可下载原图查看'; el('retry').hidden = false; };
            if (!retained) image.src = objectUrl;
            el('stage').replaceChildren(...(panel ? [panel, image] : [image]));
            if (retained) image.onload();
        } catch (error) { if (token !== generation || error.name === 'AbortError') return; el('status').textContent = error.message; el('retry').hidden = false; }
        renderDiagnostics(); renderDebugOverlay();
    }
    function renderDiagnostics() {
        const box = el('diagnostics-output'); if (!box || !el('diagnostics').open) return;
        const avg = stats.durations.length ? `${Math.round(stats.durations.reduce((a,b)=>a+b,0)/stats.durations.length)} ms` : '未知';
        const extract = stats.extracts.length ? `${Math.round(stats.extracts.reduce((a,b)=>a+b,0)/stats.extracts.length)} ms` : '未知';
        const list=items(); let queued=0;
        const diagnosticsIndexes=[];
        for(let step=1;step<=settings.count&&diagnosticsIndexes.length<settings.count;step++){const index=selected+step*direction;if(index>=0&&index<list.length)diagnosticsIndexes.push(index);}
        for(const index of diagnosticsIndexes){if(!cache.has(index)&&!pending.has(index)&&!attempted.has(index))queued++;}
        const background=[...pending.values()].filter(task=>!task.priority).length;
        box.textContent = `预取队列 ${queued} · 缩略图队列 ${thumbQueue.length} · 活跃请求 ${pending.size} · 后台并发 ${background}/${settings.concurrency} · 缓存 ${cache.size}/${settings.budget} 张（约 ${(stats.cacheBytes/1048576).toFixed(1)} MiB） · 命中 ${stats.hits} · 未命中 ${stats.misses} · 失败 ${stats.failures} · 请求平均 ${avg} · 服务器解压平均 ${extract}`;
    }
    function renderDebugOverlay() {
        const box = el('debug-output'); if (!box) return;
        const list = items();
        const current = list[selected];
        const indexes = [];
        for (let step = 1; step <= settings.count && indexes.length < settings.count; step++) {
            const index = selected + step * direction;
            if (index >= 0 && index < list.length) indexes.push(index);
        }
        const state = index => {
            if (cache.has(index)) return '缓存';
            if (pending.has(index)) return pending.get(index)?.priority ? '当前请求' : '请求中';
            if (attempted.has(index)) return '已尝试';
            return '待调度';
        };
        const sourceMode = item => directSource(item) ? '直链' : '代理';
        const lines = [`当前: ${selected + 1}/${list.length} ${current ? title(current) : '—'} [${current ? sourceMode(current) : '—'}]`,
            `方向: ${direction > 0 ? '向后' : '向前'} · 窗口: ${settings.count} · 并发: ${settings.concurrency}`];
        if (!indexes.length) lines.push('邻近预取: 无');
        else for (const index of indexes) {
            const item = list[index];
            lines.push(`${index + 1}. ${state(index)} · ${sourceMode(item)} · ${title(item)}`);
        }
        box.textContent = lines.join('\n');
    }
    document.addEventListener('click', event => {
        const button = event.target.closest('[data-gallery-image], .zip-image');
        if (!button || event.target.closest('.favorite-marker')) return;
        if (event.target.closest('a')) event.preventDefault();
        let index = findIndex(items(), identity(button)); if (index < 0) return;
        if (!dialog.open) { direction = 1; selected = index; }
        dialog.showModal();
        if (dialog.dataset.galleryIndexUrl && !el('index').childElementCount) {
            const openGeneration = generation;
            el('status').textContent = '正在读取目录图片列表…';
            populateDirectoryIndex(identity(button)).then(found => { if (!dialog.open || generation !== openGeneration) return; if (found >= 0) show(found); else el('status').textContent='此图片已不在当前目录中'; }).catch(error => { if (dialog.open && generation === openGeneration && error.name !== 'AbortError') el('status').textContent=error.message; });
        } else show(index);
    });
    document.querySelector('[data-gallery-open]')?.addEventListener('click', () => {
        document.querySelector('[data-gallery-image], .zip-image')?.click();
    });
    // Resume follows the stable file identity, loading metadata pages as needed.
    document.addEventListener('click', async event => {
        const button = event.target.closest('[data-gallery-resume]'); if (!button) return;
        const saved = savedProgress(); if (!saved) return;
        dialog.showModal();
        if (dialog.dataset.galleryIndexUrl && !el('index').childElementCount) {
            try { await populateDirectoryIndex(saved.identity); } catch (error) { dialog.showModal(); el('status').textContent = error.message; return; }
        }
        let index = findIndex(items(), saved.identity);
        while (index < 0 && document.querySelector('[data-load-more][href]')) {
            button.disabled = true; const loaded = await waitForPage(); button.disabled = false;
            if (!loaded) break;
            index = findIndex(items(), saved.identity);
        }
        if (index >= 0) show(index);
        else { button.hidden = true; dialog.showModal(); el('status').textContent = `上次浏览的图片“${saved.name}”已无法定位。关闭查看器后可继续浏览当前目录。`; }
    });
    el('prev').onclick = () => show(selected - 1); el('next').onclick = () => show(selected + 1);
    el('retry').onclick = () => { const old = cache.get(selected); if (old) { URL.revokeObjectURL(old.url); stats.cacheBytes -= old.size; cache.delete(selected); } attempted.delete(selected); show(selected); };
    el('close').onclick = () => dialog.close();
    el('thumb-toggle')?.addEventListener('click', () => { const rail = el('thumbs'); rail.hidden = !rail.hidden; el('thumb-toggle').setAttribute('aria-expanded', String(!rail.hidden)); if (!rail.hidden) renderThumbs(); else { thumbController?.abort(); thumbQueue=[]; } });
    el('jump-form')?.addEventListener('submit', async event => {
        event.preventDefault(); const value = Number(el('jump-input').value);
        if (dialog.dataset.galleryIndexUrl && !el('index').childElementCount) {
            try { await populateDirectoryIndex(identity(items()[selected])); } catch (error) { el('jump-message').textContent=error.message; return; }
        }
        while (value > items().length && document.querySelector('[data-load-more][href]')) { if (!await waitForPage()) break; }
        const count = items().length;
        if (!Number.isInteger(value) || value < 1 || value > count) { el('jump-message').textContent = `请输入 1 到 ${count} 之间的序号`; return; }
        el('jump-message').textContent = ''; show(value - 1);
    });
    el('settings-form')?.addEventListener('submit', event => { event.preventDefault(); settings = {count: clamp(el('setting-count').value,0,50,9), concurrency: clamp(el('setting-concurrency').value,1,12,9), budget: clamp(el('setting-budget').value,1,200,30)}; try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings)); } catch {} cancelOutside(); enforceConcurrency(); trimCache(); pumpPrefetch(); });
    try { exifVisible = localStorage.getItem('galleryExifVisible') === '1'; } catch {}
    const exifToggle = el('exif-toggle');
    exifToggle?.addEventListener('click', () => {
        exifVisible = !exifVisible;
        try { localStorage.setItem('galleryExifVisible', exifVisible ? '1' : '0'); } catch {}
        exifToggle.setAttribute?.('aria-pressed', String(exifVisible));
        renderExif(cache.get(selected)?.exif || null);
    });
    el('settings-reset')?.addEventListener('click', () => { settings = {...DEFAULTS}; try { localStorage.removeItem(SETTINGS_KEY); } catch {} el('setting-count').value = settings.count; el('setting-concurrency').value = settings.concurrency; el('setting-budget').value = settings.budget; cancelOutside(); enforceConcurrency(); trimCache(); pumpPrefetch(); });
    el('diagnostics')?.addEventListener('toggle', renderDiagnostics);
    el('debug-toggle')?.addEventListener('click', () => {
        const box = el('debug-output');
        const open = box?.hidden;
        if (!box) return;
        box.hidden = !open;
        el('debug-toggle')?.setAttribute?.('aria-expanded', String(open));
        if (open) renderDebugOverlay();
    });
    el('diagnostics-reset')?.addEventListener('click', () => { stats.hits=stats.misses=stats.failures=0; stats.durations=[]; stats.extracts=[]; renderDiagnostics(); });
    if (el('setting-count')) el('setting-count').value = settings.count;
    if (el('setting-concurrency')) el('setting-concurrency').value = settings.concurrency;
    if (el('setting-budget')) el('setting-budget').value = settings.budget;
    exifToggle?.setAttribute?.('aria-pressed', String(exifVisible));
    const resumeButton = document.querySelector('[data-gallery-resume]');
    if (resumeButton) resumeButton.hidden = !savedProgress();
    dialog.addEventListener('keydown', event => {
        if (event.target?.matches?.('input,textarea,select,[contenteditable="true"]')) return;
        if (event.key === 'ArrowLeft') { event.preventDefault(); show(selected-1); } else if (event.key === 'ArrowRight') { event.preventDefault(); show(selected+1); }
    });
    el('stage').addEventListener('touchstart', event => { touchStart = !viewport?.isZoomed() && event.touches.length === 1 ? event.touches[0].clientX : undefined; }, {passive:true});
    el('stage').addEventListener('touchend', event => { if (viewport?.isZoomed()) {touchStart=undefined; return;} if (touchStart === undefined) return; const dx=event.changedTouches[0].clientX-touchStart; touchStart=undefined; if (Math.abs(dx)>60) show(selected+(dx<0?1:-1)); }, {passive:true});
    el('stage').addEventListener('touchcancel',()=>{touchStart=undefined;},{passive:true});
    dialog.addEventListener('close', () => { viewport?.close(); thumbController?.abort(); thumbQueue=[]; if(indexController){indexGeneration++;indexController.abort();indexController=null;indexPromise=null;indexComplete=false;el('index').replaceChildren();} touchStart=undefined; ++generation; prefetchReady=false; attempted.clear(); for (const task of pending.values()) task.controller.abort(); pending.clear(); for (const value of cache.values()) URL.revokeObjectURL(value.url); cache.clear(); stats.cacheBytes=0; const panel=el('exif-panel'); el('stage').replaceChildren(); if(panel) el('stage').append(panel); renderExif(null); const debug=el('debug-output'); if(debug) debug.hidden=true; const toggle=el('debug-toggle'); toggle?.setAttribute?.('aria-expanded','false'); });
    // Restore exact progress by stable identity when the dialog opens with an explicit resume action.
})();
