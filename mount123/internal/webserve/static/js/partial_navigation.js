// Keep the Finder shell mounted while replacing a directory/archive view.
(() => {
    let active = null;
    const isModifierClick = event => event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
    const targetURL = link => {
        try {
            const url = new URL(link.href, location.href);
            return url.origin === location.origin && (url.pathname === '/browse' || url.pathname.startsWith('/archive/')) ? url : null;
        } catch (_) { return null; }
    };
    const resetPreview = () => {
        const pane = document.getElementById('preview-pane'); if (!pane) return;
        pane.querySelector('.preview-empty')?.removeAttribute('hidden');
        const stage = pane.querySelector('#preview-stage'); if (stage) { stage.hidden = true; stage.replaceChildren(); }
        pane.querySelector('#preview-title')?.setAttribute('hidden', '');
        pane.querySelector('#preview-meta')?.setAttribute('hidden', '');
        pane.querySelector('#preview-actions')?.setAttribute('hidden', '');
    };
    const apply = (documentRoot, url, push) => {
        const current = document.querySelector('.finder-main');
        const incoming = documentRoot.querySelector('.finder-main');
        const currentBrowse = current?.querySelector('.browse-files');
        const incomingBrowse = incoming?.querySelector('.browse-files');
        if (!current || !incoming || !currentBrowse || !incomingBrowse) return false;
        const incomingPath = incoming.querySelector('.browser-path');
        const currentPath = current.querySelector('.browser-path');
        if (incomingPath && currentPath) currentPath.replaceChildren(...[...incomingPath.childNodes].map(node => document.importNode(node, true)));
        currentBrowse.replaceChildren(...[...incomingBrowse.childNodes].map(node => document.importNode(node, true)));
        const incomingHeading = incoming.querySelector('.directory-heading h1');
        const currentHeading = document.querySelector('.toolbar-directory-title h1');
        if (incomingHeading && currentHeading) currentHeading.textContent = incomingHeading.textContent;
        const incomingCount = incoming.querySelector('[data-file-count]');
        const currentCount = document.querySelector('[data-file-count]');
        if (incomingCount && currentCount) currentCount.textContent = incomingCount.textContent;
        document.title = documentRoot.title;
        resetPreview();
        document.querySelector('#context-menu')?.setAttribute('hidden', '');
        document.querySelector('#zip-gallery[open]')?.close();
        try { if (typeof switchView === 'function') switchView(localStorage.getItem('finderViewMode') || 'icons'); } catch (_) {}
        window.updateStatusBar?.();
        if (push) history.pushState({partial: true}, '', url.href);
        window.scrollTo({top: 0, left: 0, behavior: 'instant'});
        document.dispatchEvent(new CustomEvent('cloudarchive:navigated', {detail: {url: url.href}}));
        document.dispatchEvent(new CustomEvent('files:appended', {detail: {url: url.href}}));
        return true;
    };
    async function navigate(url, push = true) {
        active?.abort();
        const controller = new AbortController(); active = controller;
        document.querySelector('.finder-main')?.classList.add('is-navigating');
        try {
            const response = await fetch(url.href, {headers: {'X-Requested-With': 'XMLHttpRequest'}, signal: controller.signal});
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const html = await response.text();
            const parsed = new DOMParser().parseFromString(html, 'text/html');
            if (!apply(parsed, url, push)) throw new Error('页面结构不支持局部导航');
        } catch (error) {
            if (error.name !== 'AbortError') window.location.assign(url.href);
        } finally {
            document.querySelector('.finder-main')?.classList.remove('is-navigating');
            if (active === controller) active = null;
        }
    }
    document.addEventListener('click', event => {
        if (isModifierClick(event)) return;
        const link = event.target.closest('a[href]');
        if (!link || event.defaultPrevented || link.hasAttribute('download') || link.target === '_blank') return;
        if (event.target.closest('[data-gallery-image], .zip-image, .favorite-marker')) return;
        const url = targetURL(link); if (!url) return;
        event.preventDefault(); navigate(url);
    }, true);
    window.addEventListener('popstate', () => navigate(new URL(location.href), false));
})();
