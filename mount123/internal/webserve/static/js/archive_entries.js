// Incrementally fills archive directories from the JSON entry endpoint. The
// page shell stays small and only the current directory rows become DOM nodes.
(() => {
    let state = null;
    const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
    const rowHTML = (row, list) => {
        const folder = row.is_dir || row.nested;
        const cls = folder ? (row.is_dir ? 'folder' : 'archive') : (row.icon || 'file');
        const icon = row.fa_icon || 'fas fa-file';
        const suffix = folder ? '/' : '';
        const attrs = `data-member-id="${esc(row.href)}" data-size="${esc(row.size)}" data-kind="${esc(row.kind)}" data-updated="—" data-favorite-key="${esc(row.favorite_key)}" data-favorite-member-path="${esc(row.member_path)}" data-favorite-name="${esc(row.name)}" data-favorite-url="${esc(row.href)}" data-download="${esc(row.download)}" data-file-type="${folder ? 'folder' : 'file'}"${row.is_image ? ` data-gallery-image data-name="${esc(row.name)}" data-url="${esc(row.href)}"` : ''}`;
        if (list) return `<div class="file-list-row file-item" ${attrs}><a class="file-list-name favorite-file-link" href="${esc(row.href)}"><span class="file-list-icon"><i class="${esc(icon)}" aria-hidden="true"></i></span>${esc(row.name)}${suffix}${row.encrypted ? ' <span class="badge bg-secondary">加密</span>' : ''}</a><div class="file-list-date">—</div><div class="file-list-meta">${folder ? '—' : esc(row.size)}</div><div class="file-list-meta">${esc(row.kind)}</div></div>`;
        return `<div class="file-grid-item file-item" ${attrs}><a class="favorite-file-link" href="${esc(row.href)}"><div class="file-grid-icon ${cls}"><i class="${esc(icon)}" aria-hidden="true"></i></div><div class="file-grid-name">${esc(row.name)}${suffix}${row.encrypted ? ' <span class="badge bg-secondary">加密</span>' : ''}</div><span class="file-grid-meta">${folder ? (row.nested ? '压缩包' : '文件夹') : esc(row.size)}</span></a></div>`;
    };
    const current = session => state === session && !session.controller.signal.aborted && session.sentinel.isConnected;
    function dispose() {
        if (!state) return;
        state.controller.abort();
        state.observer?.disconnect();
        clearTimeout(state.timer);
        state = null;
    }
    const INDEX_WAIT_MS = 90000, INDEX_ATTEMPTS = 8;
    function retryDelay(response, attempt) {
        const value = response.headers.get('Retry-After');
        if (value && /^\d+$/.test(value.trim())) {
            const seconds = Number(value.trim());
            if (Number.isFinite(seconds)) return Math.max(1000, seconds * 1000);
        }
        if (value && /^[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT$/.test(value.trim())) {
            const until = Date.parse(value.trim());
            if (Number.isFinite(until) && until > Date.now()) return Math.max(1000, until - Date.now());
        }
        return Math.min(1000 * 2 ** (attempt - 1), 8000);
    }
    function waitForIndex(session, signal, delay) {
        return new Promise((resolve, reject) => {
            const abort = () => {
                clearTimeout(timer);
                if (session.timer === timer) session.timer = null;
                reject(new DOMException('Aborted', 'AbortError'));
            };
            const timer = setTimeout(() => {
                if (session.timer === timer) session.timer = null;
                signal.removeEventListener('abort', abort);
                resolve();
            }, delay);
            session.timer = timer;
            signal.addEventListener('abort', abort, {once: true});
            if (signal.aborted) abort();
        });
    }
    function loadNext(retry = false) {
        const session = state;
        if (!session || !current(session) || (!session.cursor && session.complete)) return Promise.resolve(false);
        // Gallery and scroll restoration share the observer's in-flight page.
        if (session.pending) return session.pending;
        if (!retry) { session.failures = 0; session.retryExhausted = false; }
        clearTimeout(session.timer); session.timer = null;
        const request = (async () => {
            let succeeded = false, expired = false, waited = false;
            const controller = new AbortController(), started = Date.now();
            let deadline = null;
            const abort = () => { clearTimeout(deadline); controller.abort(); };
            session.controller.signal.addEventListener('abort', abort, {once: true});
            deadline = setTimeout(() => { expired = true; controller.abort(); }, INDEX_WAIT_MS);
            const url = new URL(session.url, location.href);
            url.searchParams.set('cursor', session.cursor);
            try {
                let page, attempts = 0;
                for (;;) {
                    if (!current(session)) return false;
                    url.searchParams.set('cursor', session.cursor);
                    const response = await fetch(url, {headers: {'X-Requested-With': 'XMLHttpRequest'}, signal: controller.signal});
                    if (!response.ok) {
                        const error = new Error(`HTTP ${response.status}`);
                        error.terminal = response.status >= 400 && response.status < 500;
                        throw error;
                    }
                    page = await response.json();
                    if (!current(session)) return false;
                    if (expired) {
                        const error = new Error('Archive index is still pending');
                        error.indexPending = true;
                        throw error;
                    }
                    const pending = response.status === 202 && page.code === 'archive_index_pending' && page.retryable === true;
                    if (response.status === 202 && !pending) {
                        const error = new Error('Invalid archive pending response');
                        error.terminal = true;
                        throw error;
                    }
                    const growing = response.status === 200 && page.complete === false && !page.items?.length;
                    if (!pending && !growing) break;
                    const delay = retryDelay(response, ++attempts);
                    if (attempts >= INDEX_ATTEMPTS || delay >= INDEX_WAIT_MS - (Date.now() - started)) {
                        const error = new Error('Archive index is still pending');
                        error.indexPending = true;
                        throw error;
                    }
                    // A pending response has no new cursor or rows. A verified
                    // progressive prefix may update its scan cursor while empty.
                    if (!pending && page.next_cursor) session.cursor = page.next_cursor;
                    waited = true;
                    const status = document.getElementById('statusText');
                    if (status) status.textContent = '正在读取目录…';
                    session.observer?.unobserve(session.sentinel);
                    await waitForIndex(session, controller.signal, delay);
                }
                if (page.items?.length) document.querySelector('.browse-files .empty-state')?.remove();
                else if (page.complete !== false) {
                    const emptyTitle = document.querySelector('.browse-files .empty-state-title');
                    if (emptyTitle) emptyTitle.textContent = '文件夹为空';
                }
                for (const row of page.items || []) {
                    if (session.seen.has(row.href)) continue;
                    session.seen.add(row.href);
                    session.grid?.insertAdjacentHTML('beforeend', rowHTML(row, false));
                    session.list?.insertAdjacentHTML('beforeend', rowHTML(row, true));
                }
                session.complete = page.complete !== false;
                session.cursor = page.next_cursor || (session.complete ? '' : session.cursor);
                session.dialog.dataset.archiveNextCursor = session.cursor;
                session.dialog.dataset.archiveComplete = String(session.complete);
                session.failures = 0; session.retryExhausted = false;
                if (session.lastError || waited) {
                    const status = document.getElementById('statusText'); if (status) status.textContent = '就绪';
                    session.lastError = false;
                }
                if (!session.cursor && session.complete) { session.sentinel.remove(); session.observer?.disconnect(); }
                succeeded = true;
                document.dispatchEvent(new CustomEvent('files:appended', {detail: {url: url.href}}));
                return true;
            } catch (error) {
                // Navigation owns cancellation; a late response must not notify
                // consumers that are waiting on a different directory.
                if (current(session) && (error.name !== 'AbortError' || expired)) {
                    session.lastError = true;
                    const pending = error.indexPending || expired;
                    const delay = !pending && !error.terminal && session.failures++ < 3 ? 1000 * 2 ** (session.failures - 1) : 0;
                    const status = document.getElementById('statusText');
                    if (status) status.textContent = pending ? (session.seen.size ? '正在读取目录，滚动后重试' : '目录仍在读取，请刷新后重试') : (delay ? '加载失败，正在重试…' : '加载失败，滚动后重试');
                    if (delay) session.timer = setTimeout(() => { session.timer = null; if (current(session)) loadNext(true); }, delay);
                    else {
                        session.retryExhausted = true;
                        // Ignore the initial visible callback. Leaving and
                        // re-entering the preload area starts a new retry budget.
                        session.observer?.observe(session.sentinel);
                    }
                    document.dispatchEvent(new CustomEvent('files:load-complete', {detail: {ok: false, error, url: url.href}}));
                }
                return false;
            } finally {
                clearTimeout(deadline);
                session.controller.signal.removeEventListener('abort', abort);
                if (state === session) session.pending = null;
                if (succeeded && state === session && !session.controller.signal.aborted) {
                    document.dispatchEvent(new CustomEvent('files:load-complete', {detail: {ok: true, url: url.href}}));
                    if (current(session)) {
                        session.observer?.unobserve(session.sentinel);
                        session.observer?.observe(session.sentinel);
                    }
                }
            }
        })();
        session.pending = request;
        return request;
    }
    function init() {
        dispose();
        const dialog = document.getElementById('zip-gallery');
        const sentinel = document.querySelector('[data-archive-sentinel]');
        if (!dialog || !sentinel || !dialog.dataset.archiveEntriesUrl) return;
        state = {
            url: dialog.dataset.archiveEntriesUrl, cursor: dialog.dataset.archiveNextCursor || '',
            complete: dialog.dataset.archiveComplete === 'true', sentinel, dialog, failures: 0, retryExhausted: false,
            controller: new AbortController(), pending: null, timer: null,
            grid: document.getElementById('fileContainer'), list: document.getElementById('fileListView'),
            seen: new Set([...document.querySelectorAll('#fileContainer .file-item')].map(item => item.dataset.memberId)),
        };
        const session = state;
        if (!session.cursor && session.complete) { sentinel.remove(); return; }
        if ('IntersectionObserver' in window) {
            const root = document.querySelector('.browse-files');
            session.observer = new IntersectionObserver(entries => {
                if (!current(session)) return;
                if (entries.some(entry => !entry.isIntersecting)) { session.retryExhausted = false; session.failures = 0; }
                if (!session.retryExhausted && entries.some(entry => entry.isIntersecting)) loadNext(true);
            }, {root, rootMargin: '700px 0px', threshold: 0});
            session.observer.observe(sentinel);
        }
    }
    window.loadArchiveNextPage = loadNext;
    window.hasArchiveNextPage = () => Boolean(state && current(state) && (state.cursor || !state.complete));
    window.initArchiveEntries = init;
    document.addEventListener('cloudarchive:before-navigate', dispose);
    document.addEventListener('cloudarchive:navigated', init);
    window.addEventListener('pagehide', dispose);
    window.addEventListener('pageshow', event => { if (event.persisted) init(); });
    init();
})();
