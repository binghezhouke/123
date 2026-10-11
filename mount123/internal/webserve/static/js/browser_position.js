// Save file identities and the number of loaded pages, never cached HTML or cursor URLs.
(() => {
    if (!document.querySelector('[data-file-page]')) return;

    const PREFIX = 'cloudarchive.position:';
    const MAX_SAVED_LOCATIONS = 50;
    const MAX_PAGES = 100;
    const MAX_AGE = 24 * 60 * 60 * 1000;
    const locationKey = new URL(location.href);
    for (const name of ['refresh', 'snapshot', 'offset', 'last_file_id']) locationKey.searchParams.delete(name);
    locationKey.searchParams.sort();
    const key = `${PREFIX}${locationKey.pathname}${locationKey.search}`;
    let selected = null;
    let restoring = false;
    let restorePromise = null;
    let loadedPages = 0;

    const scrollElement = () => document.querySelector('.finder-main') || document.scrollingElement;
    function pruneStoredLocations() {
        try {
            const keys = [];
            for (let index = 0; index < sessionStorage.length; index++) {
                const candidate = sessionStorage.key(index);
                if (candidate?.startsWith(PREFIX)) keys.push(candidate);
            }
            const states = keys.map(candidate => {
                try { return [candidate, JSON.parse(sessionStorage.getItem(candidate))?.at || 0]; }
                catch { return [candidate, 0]; }
            }).sort((a, b) => b[1] - a[1]);
            for (const [oldKey] of states.slice(MAX_SAVED_LOCATIONS)) sessionStorage.removeItem(oldKey);
        } catch { /* Storage may be disabled. */ }
    }
    function save() {
        if (restoring) return;
        try {
            const state = {pages: Math.min(loadedPages, MAX_PAGES), selected, top: scrollElement()?.scrollTop || 0, at: Date.now()};
            sessionStorage.setItem(key, JSON.stringify(state));
            pruneStoredLocations();
        } catch { /* Browsing continues with unavailable storage. */ }
    }
    function clearSelectionMark() {
        document.querySelectorAll('.file-item.browser-restored-selection').forEach(row => {
            row.classList.remove('browser-restored-selection');
            row.removeAttribute('aria-current');
        });
    }

    document.addEventListener('click', event => {
        const target = event.target instanceof Element ? event.target : event.target?.parentElement;
        const row = target?.closest('.file-item');
        if (row) { clearSelectionMark(); selected = row.dataset.fileId || null; save(); }
    }, true);
    document.addEventListener('files:appended', () => {
        loadedPages = Math.min(MAX_PAGES, loadedPages + 1);
        save();
    });
    window.addEventListener('pagehide', save);

    function readState() {
        try {
            const state = JSON.parse(sessionStorage.getItem(key));
            if (!state || Date.now() - state.at > MAX_AGE || !Number.isFinite(state.at)) return null;
            // Migrate the earlier format, which stored cursor URLs, without trusting those cursors.
            const count = Array.isArray(state.pages) ? state.pages.length : Number(state.pages);
            if (!Number.isFinite(count) || count < 0) return null;
            return {pages: Math.min(Math.floor(count), MAX_PAGES), selected: typeof state.selected === 'string' ? state.selected : null,
                top: Number.isFinite(Number(state.top)) ? Math.max(0, Number(state.top)) : 0};
        } catch { return null; }
    }
    function delay(ms) { return new Promise(resolve => setTimeout(resolve, ms)); }
    async function waitUntilIdle(button) {
        const deadline = Date.now() + 15000;
        while (button.isConnected && button.getAttribute('aria-busy') === 'true' && Date.now() < deadline) await delay(50);
        return button.isConnected && button.getAttribute('aria-busy') !== 'true';
    }
    async function loadCurrentPage(button) {
        if (!await waitUntilIdle(button) || !button.hasAttribute('href')) return false;
        return new Promise(resolve => {
            let settled = false;
            const finish = ok => {
                if (settled) return;
                settled = true;
                document.removeEventListener('files:load-complete', onComplete);
                clearTimeout(timeout);
                resolve(ok);
            };
            const onComplete = event => finish(!!event.detail?.ok);
            const timeout = setTimeout(() => finish(false), 30000);
            document.addEventListener('files:load-complete', onComplete);
            // Use the current cursor. Snapshot tokens are per-render and may have changed since save.
            button.click();
        });
    }
    function markSelection(id) {
        clearSelectionMark();
        selected = id;
        if (!id) return;
        const rows = [...document.querySelectorAll('.file-item')].filter(item => item.dataset.fileId === id);
        for (const row of rows) {
            row.classList.add('browser-restored-selection');
            row.setAttribute('aria-current', 'true');
        }
    }
    async function restore() {
        if (restorePromise) return restorePromise;
        restorePromise = (async () => {
            if (new URL(location.href).searchParams.has('refresh')) return;
            const state = readState();
            if (!state) return;
            restoring = true;
            const button = document.querySelector('[data-load-more][href]');
            for (let page = loadedPages; page < state.pages; page++) {
                if (!button || !await loadCurrentPage(button)) break;
            }
            markSelection(state.selected);
            requestAnimationFrame(() => {
                const scroller = scrollElement();
                if (scroller) scroller.scrollTop = state.top;
                restoring = false;
                save();
            });
        })().finally(() => { restorePromise = null; });
        return restorePromise;
    }

    // Normal loads restore once; BFCache restores keep their DOM and only fill missing pages.
    if (document.readyState === 'loading' || document.readyState === 'interactive') {
        document.addEventListener('DOMContentLoaded', restore, {once: true});
    } else {
        restore();
    }
    window.addEventListener('pageshow', event => { if (event.persisted) restore(); });
})();
