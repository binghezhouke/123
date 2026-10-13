// Keep the Finder shell mounted while replacing a directory/archive view.
(() => {
    let active = null, restoration = null;
    let renderedURL = location.href;
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
    const positionKey = url => {
        const copy = new URL(url, location.href);
        for (const name of ['offset', 'cursor', 'refresh', 'snapshot', 'last_file_id']) copy.searchParams.delete(name);
        copy.searchParams.sort();
        return `cloudarchive.nav-position:${copy.pathname}?${copy.searchParams.toString()}`;
    };
    const positionElements = () => [document.querySelector('.finder-main'), document.querySelector('.browse-files')].filter(Boolean);
    const savePosition = url => {
        try {
            const values = positionElements().map(element => [element.scrollTop, element.scrollLeft]);
            sessionStorage.setItem(positionKey(url), JSON.stringify(values));
        } catch (_) { /* Storage may be disabled. */ }
    };
    const frame = () => new Promise(resolve => requestAnimationFrame(resolve));
    const restorePosition = async url => {
        const controller = new AbortController(); restoration = controller;
        let saved = null;
        try { saved = JSON.parse(sessionStorage.getItem(positionKey(url)) || 'null'); } catch (_) {}
        const positions = Array.isArray(saved) ? saved : [];
        let emptyPages = 0;
        try {
            // The saved position may be below the first metadata page. Keep
            // restoring after append until it fits, then stop fetching pages.
            while (!controller.signal.aborted) {
                await frame();
                if (controller.signal.aborted) return;
                const elements = positionElements();
                let reached = true;
                for (const [index, element] of elements.entries()) {
                    const value = positions[index];
                    const top = Array.isArray(value) ? Math.max(0, Number(value[0]) || 0) : 0;
                    const left = Array.isArray(value) ? Math.max(0, Number(value[1]) || 0) : 0;
                    element.scrollTop = top; element.scrollLeft = left;
                    if (element.scrollTop + 1 < top || element.scrollLeft + 1 < left) reached = false;
                }
                if (reached || !window.hasNextFilePage?.()) return;
                const height = elements.map(element => element.scrollHeight).join(',');
                if (!await window.loadNextFilePage() || controller.signal.aborted) return;
                if (height === elements.map(element => element.scrollHeight).join(',')) {
                    // Progressive indexes can produce empty pages. Pace those
                    // retries and cap a stalled restoration attempt.
                    if (++emptyPages >= 20) return;
                    await new Promise(resolve => {
                        const timer = setTimeout(done, 1000);
                        function done() { clearTimeout(timer); controller.signal.removeEventListener('abort', done); resolve(); }
                        controller.signal.addEventListener('abort', done, {once: true});
                    });
                } else emptyPages = 0;
            }
        } finally { if (restoration === controller) restoration = null; }
    };
    let saveTimer = null;
    const saveCurrentPositionSoon = () => {
        if (restoration) return;
        clearTimeout(saveTimer);
        const url = renderedURL;
        saveTimer = setTimeout(() => { saveTimer = null; savePosition(url); }, 150);
    };
    const apply = (documentRoot, url, push) => {
        const current = document.querySelector('.finder-main');
        const incoming = documentRoot.querySelector('.finder-main');
        const currentBrowse = current?.querySelector('.browse-files');
        const incomingBrowse = incoming?.querySelector('.browse-files');
        if (!current || !incoming || !currentBrowse || !incomingBrowse) return false;
        const incomingPath = incoming.querySelector('.browser-path');
        // The base layout moves the live breadcrumb into the toolbar during
        // the first paint, so it is no longer inside .finder-main on later
        // navigations. Update that persistent wrapper instead of looking only
        // in the replaced content subtree.
        const currentPath = document.querySelector('.toolbar-breadcrumbs') || current.querySelector('.browser-path');
        if (incomingPath && currentPath) {
            const incomingBar = incomingPath.querySelector('.path-bar');
            const currentBar = currentPath.querySelector('.path-bar');
            if (incomingBar && currentBar) currentBar.replaceChildren(...[...incomingBar.childNodes].map(node => document.importNode(node, true)));
            else currentPath.replaceChildren(...[...incomingPath.childNodes].map(node => document.importNode(node, true)));
        }
        const currentGallery = document.getElementById('zip-gallery');
        const incomingGallery = incoming.querySelector('#zip-gallery');
        if (currentGallery && incomingGallery) {
            currentGallery.dataset.galleryScope = incomingGallery.dataset.galleryScope || '';
            currentGallery.dataset.galleryIndexUrl = incomingGallery.dataset.galleryIndexUrl || '';
            currentGallery.dataset.archiveEntriesUrl = incomingGallery.dataset.archiveEntriesUrl || '';
            currentGallery.dataset.archiveNextCursor = incomingGallery.dataset.archiveNextCursor || '';
            currentGallery.dataset.archiveComplete = incomingGallery.dataset.archiveComplete || '';
        }
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
        renderedURL = url.href;
        document.dispatchEvent(new CustomEvent('cloudarchive:navigated', {detail: {url: url.href}}));
        document.dispatchEvent(new CustomEvent('files:appended', {detail: {url: url.href}}));
        restorePosition(url);
        return true;
    };
    async function navigate(url, push = true) {
        clearTimeout(saveTimer); saveTimer = null;
        // popstate has already changed location.href; the mounted content still
        // belongs to renderedURL and must be saved under that outgoing key.
        if (!restoration) savePosition(renderedURL);
        restoration?.abort(); restoration = null;
        active?.abort();
        document.dispatchEvent(new CustomEvent('cloudarchive:before-navigate'));
        const controller = new AbortController(); active = controller;
        document.querySelector('.finder-main')?.classList.add('is-navigating');
        try {
            const response = await fetch(url.href, {headers: {'X-Requested-With': 'XMLHttpRequest'}, signal: controller.signal});
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const html = await response.text();
            if (active !== controller || controller.signal.aborted) return;
            const parsed = new DOMParser().parseFromString(html, 'text/html');
            if (!apply(parsed, url, push)) throw new Error('页面结构不支持局部导航');
        } catch (error) {
            if (active === controller && !controller.signal.aborted && error.name !== 'AbortError') window.location.assign(url.href);
        } finally {
            if (active === controller) {
                document.querySelector('.finder-main')?.classList.remove('is-navigating');
                active = null;
            }
        }
    }
    document.addEventListener('click', event => {
        if (isModifierClick(event)) return;
        const link = event.target.closest('a[href]');
        if (!link || event.defaultPrevented || link.hasAttribute('download') || link.hasAttribute('data-load-more') || link.target === '_blank') return;
        if (event.target.closest('[data-gallery-image], .zip-image, .favorite-marker')) return;
        const url = targetURL(link); if (!url) return;
        event.preventDefault(); navigate(url);
    }, true);
    document.addEventListener('scroll', saveCurrentPositionSoon, {capture: true, passive: true});
    window.addEventListener('pagehide', () => { clearTimeout(saveTimer); if (!restoration) savePosition(renderedURL); restoration?.abort(); active?.abort(); });
    window.addEventListener('popstate', () => navigate(new URL(location.href), false));
})();
