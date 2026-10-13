// Append server-rendered entries so preview links and filename escaping stay consistent.
(() => {
    let session = null;
    function dispose() {
        if (!session) return;
        session.controller.abort();
        session.observer?.disconnect();
        session.button.removeEventListener?.('click', session.click);
        if (session.button.dataset) delete session.button.dataset.paginationBound;
        session.status?.remove?.();
        session = null;
    }
    function initFilePagination() {
        const button = document.querySelector('[data-load-more]');
        if (session?.button === button && !session.controller.signal.aborted) return;
        dispose();
        if (!button || button.dataset?.paginationBound) return;
        if (button.dataset) button.dataset.paginationBound = '1';
        const status = document.createElement('p');
        status.setAttribute('role', 'status');
        status.setAttribute('aria-live', 'polite');
        button.parentElement.after(status);
        const owner = {button, status, controller: new AbortController(), pending: null, observer: null};
        session = owner;
        const current = () => session === owner && !owner.controller.signal.aborted && button.isConnected !== false;
        let busy = false;
        const completed = new Set();

        function loadNext() {
            if (!current() || !button.hasAttribute('href')) return Promise.resolve(false);
            if (busy) return owner.pending;
            owner.pending = performLoad();
            return owner.pending;
        }
        owner.loadNext = loadNext;
        async function performLoad() {
            const url = button.href;
            busy = true;
            button.setAttribute('aria-disabled', 'true');
            button.setAttribute('aria-busy', 'true');
            button.textContent = '加载中…';
            status.textContent = '';
            let succeeded = false;
            try {
                const response = await fetch(url, {headers: {'X-Requested-With': 'XMLHttpRequest'}, signal: owner.controller.signal});
                if (!response.ok) throw new Error('读取失败');
                const page = new DOMParser().parseFromString(await response.text(), 'text/html');
                if (!current()) return false;
                if (!page.querySelector('[data-file-page]')) throw new Error('响应无效');
                const next = page.querySelector('[data-load-more]')?.href;
                if (next && (next === url || completed.has(next))) throw new Error('分页游标未更新');

                const scrollPositions = [document.scrollingElement, document.querySelector('.finder-main'), document.querySelector('.browse-files')]
                    .filter(Boolean).map(element => [element, element.scrollTop, element.scrollLeft]);
                for (const id of ['fileContainer', 'fileListView']) {
                    const container = document.getElementById(id);
                    if (!container) continue;
                    const seen = new Set([...container.querySelectorAll('.file-item')].map(item => item.dataset.fileId));
                    const fragment = document.createDocumentFragment();
                    page.querySelectorAll(`#${id} .file-item`).forEach(item => {
                        if (seen.has(item.dataset.fileId)) return;
                        seen.add(item.dataset.fileId);
                        fragment.append(document.importNode(item, true));
                    });
                    container.append(fragment);
                }
                completed.add(url);
                succeeded = true;
                const count = document.querySelectorAll('#fileContainer .file-item').length;
                if (count) document.querySelector('.empty-state')?.remove();
                const heading = document.querySelector('[data-file-count]');
                if (heading) heading.textContent = heading.classList.contains('badge') ? String(count) : `${count} 个项目`;
                const footer = document.getElementById('statusCount');
                if (footer) footer.textContent = `${count} 个项目`;
                status.textContent = next ? '' : '';
                if (next) {
                    button.href = next;
                    button.innerHTML = '<i class="fas fa-chevron-right" aria-hidden="true"></i><span>加载更多</span>';
                } else {
                    button.removeAttribute('href');
                    button.hidden = true;
                    status.hidden = true;
                    observer?.disconnect();
                }
                for (const [element, top, left] of scrollPositions) {
                    element.scrollTop = top;
                    element.scrollLeft = left;
                }
                document.dispatchEvent(new CustomEvent('files:appended', {detail: {url}}));
            } catch (error) {
                if (!current() || error.name === 'AbortError') return false;
                status.textContent = '加载失败，点击此处重试。';
                button.innerHTML = '<i class="fas fa-rotate-right" aria-hidden="true"></i><span>重试加载</span>';
                button.closest('[data-load-sentinel]')?.classList.add('retry');
            } finally {
                busy = false; owner.pending = null;
                if (current()) {
                    document.dispatchEvent(new CustomEvent('files:load-complete', {detail: {ok: succeeded}}));
                    button.removeAttribute('aria-busy');
                    if (button.hasAttribute('href')) button.removeAttribute('aria-disabled');
                    // Appending a short page may leave the sentinel inside the preload
                    // margin. Observe it afresh after releasing busy so another page
                    // loads without requiring an exit/re-entry intersection transition.
                    if (current() && succeeded && button.hasAttribute('href') && observer) {
                        observer.unobserve(sentinel);
                        observer.observe(sentinel);
                    }
                }
            }
            return succeeded;
        }

        owner.click = event => { event.preventDefault(); loadNext(); };
        button.addEventListener('click', owner.click);

        const sentinel = button.closest('[data-load-sentinel]') || button.parentElement;
        let observer = null;
        if ('IntersectionObserver' in window && sentinel) {
            observer = new IntersectionObserver(entries => {
                if (entries.some(entry => entry.isIntersecting)) loadNext();
            }, {root: document.querySelector('.browse-files') || null, rootMargin: '600px 0px', threshold: 0});
            owner.observer = observer;
            observer.observe(sentinel);
        }
    }

    // Delegation also covers entries appended after the first page.
    document.addEventListener('mouseover', event => {
        const item = event.target.closest('.file-item');
        const label = document.getElementById('statusText');
        if (item && label) label.textContent = item.querySelector('.file-grid-name, .file-list-name')?.textContent.trim() || '项目';
    });
    document.addEventListener('mouseout', event => {
        const label = document.getElementById('statusText');
        if (event.target.closest('.file-item') && label) label.textContent = '就绪';
    });
    window.hasNextFilePage = () => window.hasArchiveNextPage?.() || Boolean(session && !session.controller.signal.aborted && session.button.hasAttribute('href'));
    window.loadNextFilePage = () => window.hasArchiveNextPage?.() ? window.loadArchiveNextPage() : session?.loadNext() || Promise.resolve(false);
    globalThis.initFilePagination = initFilePagination;
    document.addEventListener('cloudarchive:before-navigate', dispose);
    window.addEventListener('pagehide', dispose);
    window.addEventListener('pageshow', event => { if (event.persisted) initFilePagination(); });
    initFilePagination();
    document.addEventListener('cloudarchive:navigated', initFilePagination);
})();
