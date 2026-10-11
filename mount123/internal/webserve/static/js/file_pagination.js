// Append server-rendered entries so preview links and filename escaping stay consistent.
(() => {
    const button = document.querySelector('[data-load-more]');
    if (!button) return;
    const status = document.createElement('p');
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    button.parentElement.after(status);
    let busy = false;
    const completed = new Set();

    button.addEventListener('click', async event => {
        event.preventDefault();
        if (busy || !button.hasAttribute('href')) return;
        const url = button.href;
        busy = true;
        button.setAttribute('aria-disabled', 'true');
        button.setAttribute('aria-busy', 'true');
        button.textContent = '加载中…';
        status.textContent = '';
        let succeeded = false;
        try {
            const response = await fetch(url, {headers: {'X-Requested-With': 'XMLHttpRequest'}});
            if (!response.ok) throw new Error('读取失败');
            const page = new DOMParser().parseFromString(await response.text(), 'text/html');
            if (!page.querySelector('[data-file-page]')) throw new Error('响应无效');
            const next = page.querySelector('[data-load-more]')?.href;
            if (next && (next === url || completed.has(next))) throw new Error('分页游标未更新');

            const scrollPositions = [document.scrollingElement, document.querySelector('.finder-main')]
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
            status.textContent = next ? `已加载 ${count} 个项目` : '';
            if (next) {
                button.href = next;
                button.textContent = '加载更多';
            } else {
                button.removeAttribute('href');
                button.hidden = true;
                status.hidden = true;
            }
            // Keep the viewport stable even when the focused load button moves down.
            for (const [element, top, left] of scrollPositions) {
                element.scrollTop = top;
                element.scrollLeft = left;
            }
            document.dispatchEvent(new CustomEvent('files:appended', {detail: {url}}));
        } catch {
            status.textContent = '加载失败，当前列表已保留，请重试。';
            button.textContent = '重试加载';
        } finally {
            busy = false;
            document.dispatchEvent(new CustomEvent('files:load-complete', {detail: {ok: succeeded}}));
            button.removeAttribute('aria-busy');
            if (button.hasAttribute('href')) button.removeAttribute('aria-disabled');
        }
    });

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
})();
