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
    async function loadNext() {
        if (!state || state.busy || !state.cursor) return false;
        state.busy = true;
        try {
            const url = new URL(state.url, location.href); url.searchParams.set('cursor', state.cursor);
            const response = await fetch(url, {headers: {'X-Requested-With': 'XMLHttpRequest'}});
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const page = await response.json();
            const grid = document.getElementById('fileContainer'), list = document.getElementById('fileListView');
            for (const row of page.items || []) {
                if (grid) grid.insertAdjacentHTML('beforeend', rowHTML(row, false));
                if (list) list.insertAdjacentHTML('beforeend', rowHTML(row, true));
            }
            state.cursor = page.next_cursor || '';
            if (!state.cursor) state.sentinel?.remove();
            document.dispatchEvent(new CustomEvent('files:appended', {detail: {url: url.href}}));
            document.dispatchEvent(new CustomEvent('files:load-complete', {detail: {ok: true}}));
            return true;
        } catch (error) {
            document.dispatchEvent(new CustomEvent('files:load-complete', {detail: {ok: false, error}}));
            return false;
        } finally { state.busy = false; }
    }
    function init() {
        const dialog = document.getElementById('zip-gallery');
        const sentinel = document.querySelector('[data-archive-sentinel]');
        if (!dialog || !sentinel || !dialog.dataset.archiveEntriesUrl || !dialog.dataset.archiveNextCursor) { state = null; return; }
        state = {url: dialog.dataset.archiveEntriesUrl, cursor: dialog.dataset.archiveNextCursor, sentinel, busy: false};
        window.loadArchiveNextPage = loadNext;
        if ('IntersectionObserver' in window) {
            const root = document.querySelector('.browse-files');
            state.observer = new IntersectionObserver(entries => { if (entries.some(entry => entry.isIntersecting)) loadNext(); }, {root, rootMargin: '700px 0px', threshold: 0});
            state.observer.observe(sentinel);
        }
    }
    window.initArchiveEntries = init;
    document.addEventListener('cloudarchive:navigated', init);
    init();
})();
