// Persist widths per view and visible-column layout, without changing file order.
(() => {
    const MIN = 72, MAX = 1200;
    document.querySelectorAll('[data-column-layout]').forEach(container => {
        const table = container.tagName === 'TABLE';
        const header = container.querySelector(table ? 'thead tr' : '.file-list-header');
        if (!header) return;
        const cells = Array.from(header.children);
        const baseKey = `cloudarchive.columns.v1.${container.dataset.columnLayout}`;
        let drag = null, profile = '', widths = null;
        let cols;
        if (table) {
            const group = document.createElement('colgroup');
            cols = cells.map(() => group.appendChild(document.createElement('col')));
            container.prepend(group);
        }
        const minimum = index => index === 0 ? 120 : MIN;
        const visible = () => cells.map((cell, i) => getComputedStyle(cell).display !== 'none' ? i : -1).filter(i => i >= 0);
        const key = () => `${baseKey}.${visible().join('-')}`;
        function render() {
            container.classList.toggle('columns-sized', !!widths);
            if (!widths) {
                container.style.removeProperty('--list-columns');
                container.style.removeProperty('--list-width');
                if (table) {container.style.removeProperty('width'); cols.forEach(col => col.style.removeProperty('width'));}
                return;
            }
            const indexes = visible();
            if (table) {
                cols.forEach((col, i) => col.style.width = `${widths[i]}px`);
                container.style.width = `${widths.reduce((sum, n) => sum + n, 0)}px`;
            } else {
                const css = getComputedStyle(header);
                const padding = parseFloat(css.paddingLeft) + parseFloat(css.paddingRight);
                const gap = parseFloat(css.columnGap) || 0;
                container.style.setProperty('--list-columns', indexes.map(i => `${widths[i]}px`).join(' '));
                container.style.setProperty('--list-width', `${indexes.reduce((sum, i) => sum + widths[i], 0) + padding + gap * (indexes.length - 1)}px`);
            }
            cells.forEach((cell, i) => cell.querySelector('.column-resizer').setAttribute('aria-valuenow', String(Math.round(widths[i]))));
        }
        function restore() {
            profile = key(); widths = null;
            try {
                const saved = JSON.parse(localStorage.getItem(profile));
                if (Array.isArray(saved) && saved.length === cells.length && saved.every((n, i) =>
                    Number.isFinite(n) && n >= minimum(i) && n <= MAX)) widths = saved;
            } catch { /* Invalid or inaccessible preferences use the responsive defaults. */ }
            render();
        }
        function save() {
            try {localStorage.setItem(profile, JSON.stringify(widths));} catch { /* Keep the current layout for this page. */ }
        }
        function measure() {
            return cells.map((cell, i) => Math.max(minimum(i), Math.min(MAX, cell.getBoundingClientRect().width)));
        }
        function end(cancel = false) {
            if (!drag) return;
            const previous = drag;
            drag = null;
            document.documentElement.classList.remove('resizing-columns');
            if (cancel) restore(); else save();
            if (previous.handle.hasPointerCapture(previous.id)) previous.handle.releasePointerCapture(previous.id);
        }
        cells.forEach((cell, index) => {
            const handle = document.createElement('span');
            handle.className = 'column-resizer';
            handle.tabIndex = 0;
            handle.setAttribute('role', 'separator');
            handle.setAttribute('aria-orientation', 'vertical');
            handle.setAttribute('aria-label', `${cell.textContent.trim()}列宽`);
            handle.setAttribute('aria-valuemin', String(minimum(index)));
            handle.setAttribute('aria-valuenow', String(minimum(index)));
            handle.setAttribute('aria-valuemax', String(MAX));
            handle.title = '拖动调整列宽；双击恢复默认；方向键微调';
            cell.append(handle);
            handle.addEventListener('pointerdown', event => {
                if (event.button !== 0 || drag) return;
                event.preventDefault();
                profile = key();
                widths = measure();
                drag = {handle, id: event.pointerId, x: event.clientX, width: widths[index]};
                handle.setPointerCapture(event.pointerId);
                document.documentElement.classList.add('resizing-columns');
                render();
            });
            handle.addEventListener('pointermove', event => {
                if (!drag || drag.handle !== handle || drag.id !== event.pointerId) return;
                widths[index] = Math.max(minimum(index), Math.min(MAX, drag.width + event.clientX - drag.x));
                render();
            });
            handle.addEventListener('pointerup', () => end());
            handle.addEventListener('pointercancel', () => end(true));
            handle.addEventListener('lostpointercapture', () => end(true));
            handle.addEventListener('dblclick', event => {
                event.preventDefault(); end(true);
                try {localStorage.removeItem(key());} catch { /* Current page can still reset. */ }
                widths = null; render();
            });
            handle.addEventListener('keydown', event => {
                if (event.key === 'Escape') {end(true); return;}
                if (!['ArrowLeft', 'ArrowRight', 'Home'].includes(event.key)) return;
                event.preventDefault();
                profile = key(); widths = measure();
                widths[index] = event.key === 'Home' ? minimum(index) : Math.max(minimum(index), Math.min(MAX,
                    widths[index] + (event.key === 'ArrowLeft' ? -1 : 1) * (event.shiftKey ? 40 : 10)));
                render(); save();
            });
            handle.addEventListener('focus', () => handle.setAttribute('aria-valuenow', String(Math.round(cell.getBoundingClientRect().width))));
        });
        restore();
        window.addEventListener('resize', () => {end(true); if (profile !== key()) restore(); else render();});
        window.addEventListener('blur', () => end(true));
        window.addEventListener('keydown', event => {if (event.key === 'Escape' && drag) end(true);});
        window.addEventListener('storage', event => {if (!drag && (event.key === key() || event.key === null)) restore();});
    });
})();
