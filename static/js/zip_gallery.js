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
    const DEFAULTS = {count: 9, concurrency: 2, budget: 256};
    const SETTINGS_KEY = 'imageGallerySettings';
    let settings = readSettings();
    let selected = 0, direction = 1, generation = 0, prefetchReady = false, touchStart;
    let thumbController = null, thumbQueue = [], indexController = null, indexPromise = null, indexComplete = false, indexGeneration = 0;

    function readSettings() {
        try {
            const value = JSON.parse(localStorage.getItem(SETTINGS_KEY) || '{}');
            return {count: clamp(value.count, 0, 50, DEFAULTS.count), concurrency: clamp(value.concurrency, 1, 6, DEFAULTS.concurrency), budget: clamp(value.budget, 16, 2048, DEFAULTS.budget)};
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
    function identity(item) { return item.dataset.fileId || item.dataset.memberId || source(item); }
    function title(item) { return item.dataset.name || item.getAttribute('aria-label') || item.textContent.trim(); }
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
    function touchCache(index, url, size) {
        cache.set(index, {url, size}); stats.cacheBytes += size;
        trimCache();
    }
    function trimCache() {
        const maxBytes = settings.budget * 1024 * 1024;
        while (cache.size > Math.max(30, settings.count + 1) || stats.cacheBytes > maxBytes) {
            const key = [...cache.keys()].find(k => k !== selected);
            if (key === undefined) break;
            const old = cache.get(key); URL.revokeObjectURL(old.url); stats.cacheBytes -= old.size; cache.delete(key);
        }
    }
    function cancelOutside() {
        for (const [idx, task] of pending) {
            const distance = (idx - selected) * direction;
            if (distance < -1 || distance > settings.count) { task.controller.abort(); pending.delete(idx); }
        }
    }
    function enforceConcurrency() {
        while ([...pending.values()].filter(task => !task.priority).length > settings.concurrency) {
            const expendable = [...pending.entries()].reverse().find(([key, task]) => key !== selected && !task.priority);
            if (!expendable) break;
            expendable[1].controller.abort(); pending.delete(expendable[0]);
        }
    }
    function load(index, priority = false, ownerSignal = null) {
        const list = items(), item = list[index];
        if (!item) return Promise.reject(new Error('图片不存在'));
        if (cache.has(index)) { stats.hits++; const value = cache.get(index); cache.delete(index); cache.set(index, value); return Promise.resolve(value.url); }
        if (pending.has(index)) { const task=pending.get(index); if (priority) {task.thumbOnly=false; task.priority=true;} return task.promise; }
        const task = {controller: new AbortController(), priority, thumbOnly:Boolean(ownerSignal)};
        if (ownerSignal) ownerSignal.addEventListener('abort', () => { if (task.thumbOnly && pending.get(index) === task) { task.controller.abort(); pending.delete(index); } }, {once:true});
        task.promise = (async () => {
        const started = typeof performance !== 'undefined' ? performance.now() : Date.now(); stats.misses++;
            try {
                const response = await fetch(source(item), {signal: task.controller.signal});
                if (response.status === 401) throw new Error('解压密码已失效或不正确，请关闭预览并重新输入密码');
                if (!response.ok || !response.headers.get('Content-Type')?.startsWith('image/')) throw new Error(`图片读取失败（HTTP ${response.status}），请重试或下载原图`);
                const timing = response.headers.get('Server-Timing') || '';
                const extract = timing.match(/(?:^|,)\s*extract;dur=([\d.]+)/i);
                if (extract) { stats.extracts.push(Number(extract[1])); if (stats.extracts.length > 100) stats.extracts.shift(); }
                const blob = await response.blob();
                if (task.controller.signal.aborted) throw new DOMException('Aborted', 'AbortError');
                const url = URL.createObjectURL(blob); touchCache(index, url, blob.size || 0);
                stats.durations.push((typeof performance !== 'undefined' ? performance.now() : Date.now()) - started); if (stats.durations.length > 100) stats.durations.shift();
                return url;
            } catch (error) { if (error.name !== 'AbortError') stats.failures++; throw error; }
        })().finally(() => { if (pending.get(index) === task) pending.delete(index); renderDiagnostics(); pumpThumbQueue(); pumpPrefetch(); });
        pending.set(index, task); return task.promise;
    }
    function pumpPrefetch() {
        if (!dialog.open || !prefetchReady) return;
        if (thumbQueue.length) { pumpThumbQueue(); return; }
        const max = settings.concurrency;
        for (let step = 1; step <= settings.count && [...pending.values()].filter(task => !task.priority).length < max; step++) {
            const index = selected + step * direction;
            if (index < 0 || index >= items().length) break;
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
        if (!dialog.open || [...pending.values()].filter(task => !task.priority).length >= settings.concurrency || !thumbQueue.length) return;
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
        el('stage').replaceChildren(); el('title').textContent = title(list[index]); el('counter').textContent = `${index + 1} / ${list.length}`;
        const url = source(list[index]); el('download').href = `${url}${url.includes('?') ? '&' : '?'}download=1`;
        el('prev').disabled = index === 0; el('next').disabled = index === list.length - 1 && !document.querySelector?.('[data-load-more][href]'); el('retry').hidden = true; el('status').textContent = '正在读取图片…'; saveProgress(list[index]);
        try {
            const objectUrl = await load(index, true); if (token !== generation || !dialog.open) return;
            const image = new Image(); image.alt = title(list[index]); image.onload = () => { if (token !== generation) return; viewport?.attach(image); el('status').textContent = '滚轮或双指缩放 · 放大后拖动 · ← → 切换图片'; prefetchReady = true; renderThumbs(); pumpPrefetch(); };
            image.onerror = () => { if (token !== generation) return; el('status').textContent = '浏览器无法显示此图片，可下载原图查看'; el('retry').hidden = false; };
            image.src = objectUrl; el('stage').replaceChildren(image);
        } catch (error) { if (token !== generation || error.name === 'AbortError') return; el('status').textContent = error.message; el('retry').hidden = false; }
        renderDiagnostics();
    }
    function renderDiagnostics() {
        const box = el('diagnostics-output'); if (!box || !el('diagnostics').open) return;
        const avg = stats.durations.length ? `${Math.round(stats.durations.reduce((a,b)=>a+b,0)/stats.durations.length)} ms` : '未知';
        const extract = stats.extracts.length ? `${Math.round(stats.extracts.reduce((a,b)=>a+b,0)/stats.extracts.length)} ms` : '未知';
        const list=items(); let queued=0;
        for(let step=1;step<=settings.count;step++){const index=selected+step*direction;if(index<0||index>=list.length)break;if(!cache.has(index)&&!pending.has(index)&&!attempted.has(index))queued++;}
        const background=[...pending.values()].filter(task=>!task.priority).length;
        box.textContent = `预取队列 ${queued} · 缩略图队列 ${thumbQueue.length} · 活跃请求 ${pending.size} · 后台并发 ${background}/${settings.concurrency} · 缓存 ${(stats.cacheBytes/1048576).toFixed(1)} MiB · 命中 ${stats.hits} · 未命中 ${stats.misses} · 失败 ${stats.failures} · 请求平均 ${avg} · 服务器解压平均 ${extract}`;
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
    el('settings-form')?.addEventListener('submit', event => { event.preventDefault(); settings = {count: clamp(el('setting-count').value,0,50,9), concurrency: clamp(el('setting-concurrency').value,1,6,2), budget: clamp(el('setting-budget').value,16,2048,256)}; try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(settings)); } catch {} cancelOutside(); enforceConcurrency(); trimCache(); pumpPrefetch(); });
    el('settings-reset')?.addEventListener('click', () => { settings = {...DEFAULTS}; try { localStorage.removeItem(SETTINGS_KEY); } catch {} el('setting-count').value = settings.count; el('setting-concurrency').value = settings.concurrency; el('setting-budget').value = settings.budget; cancelOutside(); enforceConcurrency(); trimCache(); pumpPrefetch(); });
    el('diagnostics')?.addEventListener('toggle', renderDiagnostics);
    el('diagnostics-reset')?.addEventListener('click', () => { stats.hits=stats.misses=stats.failures=0; stats.durations=[]; stats.extracts=[]; renderDiagnostics(); });
    if (el('setting-count')) el('setting-count').value = settings.count;
    if (el('setting-concurrency')) el('setting-concurrency').value = settings.concurrency;
    if (el('setting-budget')) el('setting-budget').value = settings.budget;
    const resumeButton = document.querySelector('[data-gallery-resume]');
    if (resumeButton) resumeButton.hidden = !savedProgress();
    dialog.addEventListener('keydown', event => {
        if (event.target?.matches?.('input,textarea,select,[contenteditable="true"]')) return;
        if (event.key === 'ArrowLeft') { event.preventDefault(); show(selected-1); } else if (event.key === 'ArrowRight') { event.preventDefault(); show(selected+1); }
    });
    el('stage').addEventListener('touchstart', event => { touchStart = !viewport?.isZoomed() && event.touches.length === 1 ? event.touches[0].clientX : undefined; }, {passive:true});
    el('stage').addEventListener('touchend', event => { if (viewport?.isZoomed()) {touchStart=undefined; return;} if (touchStart === undefined) return; const dx=event.changedTouches[0].clientX-touchStart; touchStart=undefined; if (Math.abs(dx)>60) show(selected+(dx<0?1:-1)); }, {passive:true});
    el('stage').addEventListener('touchcancel',()=>{touchStart=undefined;},{passive:true});
    dialog.addEventListener('close', () => { viewport?.close(); thumbController?.abort(); thumbQueue=[]; if(indexController){indexGeneration++;indexController.abort();indexController=null;indexPromise=null;indexComplete=false;el('index').replaceChildren();} touchStart=undefined; ++generation; prefetchReady=false; attempted.clear(); for (const task of pending.values()) task.controller.abort(); pending.clear(); for (const value of cache.values()) URL.revokeObjectURL(value.url); cache.clear(); stats.cacheBytes=0; el('stage').replaceChildren(); });
    // Restore exact progress by stable identity when the dialog opens with an explicit resume action.
})();
