// Shared actions bind after the shell exists and read the current location.
(() => {
    const menu = document.getElementById('context-menu');
    if (!menu) return;
    const favorite = menu.querySelector('[data-cm-action="favorite"]');
    const divider = menu.querySelector('[data-cm-favorite-separator]');
    let item = null;
    const dismiss = () => { menu.hidden = true; item = null; };
    const navigate = (changes) => {
        const url = new URL(location.href);
        for (const [key, value] of Object.entries(changes)) {
            if (value === '') url.searchParams.delete(key); else url.searchParams.set(key, value);
        }
        url.searchParams.delete('offset');
        url.searchParams.delete('last_file_id');
        location.assign(url.href);
    };
    document.addEventListener('contextmenu', event => {
        if (!event.target.closest('.browse-files')) { dismiss(); return; }
        event.preventDefault();
        item = event.target.closest('.file-item[data-favorite-key]');
        favorite.hidden = divider.hidden = !item;
        favorite.textContent = item?.querySelector('.favorite-marker.is-favorite') ? '修改收藏颜色…' : '收藏到颜色…';
        const directory = location.pathname === '/browse';
        menu.querySelectorAll('[data-cm-sort], [data-cm-arrange], [data-cm-action="probe"]').forEach(button => { button.hidden = !directory; });
        menu.hidden = false;
        const rect = menu.getBoundingClientRect();
        menu.style.left = Math.max(0, Math.min(event.clientX, innerWidth - rect.width - 8)) + 'px';
        menu.style.top = Math.max(0, Math.min(event.clientY, innerHeight - rect.height - 8)) + 'px';
    });
    document.addEventListener('click', event => {
        const header = event.target.closest('.file-list-header .sortable');
        if (header) {
            const url = new URL(location.href), sort = header.dataset.sort;
            navigate({sort, direction: (url.searchParams.get('sort') || 'name') === sort && (url.searchParams.get('direction') || 'asc') === 'asc' ? 'desc' : 'asc'});
        }
        if (!menu.contains(event.target)) dismiss();
    });
    menu.addEventListener('click', event => {
        const button = event.target.closest('button');
        if (!button || button.hidden) return;
        const selected = item;
        dismiss();
        if ('cmSort' in button.dataset) navigate({sort: button.dataset.cmSort});
        else if ('cmArrange' in button.dataset) navigate({arrange: button.dataset.cmArrange});
        else if ('cmView' in button.dataset) window.switchView?.(button.dataset.cmView);
        else if (button.dataset.cmAction === 'favorite') window.openFavoritePicker?.(selected);
        else if (button.dataset.cmAction === 'refresh') window.refreshFiles?.();
        else if (button.dataset.cmAction === 'probe') {
            const parent = new URL(location.href).searchParams.get('parent_id') || '0';
            (window.runProbe || runProbe)(parent);
        }
    });
    document.addEventListener('keydown', event => { if (event.key === 'Escape') dismiss(); });
    document.addEventListener('cloudarchive:before-navigate', dismiss);
    document.addEventListener('cloudarchive:navigated', dismiss);
    document.addEventListener('scroll', dismiss, true);

    let probeController = null;
    document.addEventListener('cloudarchive:before-navigate', () => probeController?.abort());
    async function runProbe() {
        probeController?.abort();
        const controller = new AbortController(); probeController = controller;
        const originalURL = location.href;
        const parent = new URL(originalURL).searchParams.get('parent_id') || '0';
        const status = document.getElementById('probe-archive-status') || document.getElementById('statusText');
        const setStatus = message => { if (status && !controller.signal.aborted && location.href === originalURL) status.textContent = message; };
        const request = async (path, method = 'GET') => {
            const response = await fetch(`/browse/${path}?parent_id=${encodeURIComponent(parent)}`, {method, signal: controller.signal});
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            return response;
        };
        setStatus('探测中…');
        try {
            await request('probe', 'POST');
            while (!controller.signal.aborted && location.href === originalURL) {
                const state = await (await request('probe-status')).json();
                if (state.state !== 'running' && state.state !== 'queued') {
                    setStatus(state.state === 'complete' ? '探测完成，正在刷新…' : '探测已结束');
                    if (state.state === 'complete' && !controller.signal.aborted && location.href === originalURL) location.reload();
                    return;
                }
                setStatus(`探测中… 已检查 ${state.checked || 0}/${state.total || 0}，发现 ${state.detected || 0} 个`);
                await new Promise(resolve => setTimeout(resolve, 800));
            }
        } catch (error) { if (error.name !== 'AbortError') setStatus('探测失败：' + error.message); }
        finally { if (probeController === controller) probeController = null; }
    }
})();
