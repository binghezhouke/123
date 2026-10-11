// Finder-like keyboard navigation for the directory file panes.
(() => {
    const isTyping = target => target instanceof HTMLElement &&
        (target.matches('input, textarea, select, button, [contenteditable="true"]') || target.closest('dialog[open]'));
    const visibleItems = () => {
        const list = document.getElementById('fileListView');
        const grid = document.getElementById('fileContainer');
        const active = list && getComputedStyle(list).display !== 'none' ? list : grid;
        return [...(active?.querySelectorAll('.file-item') || [])];
    };
    const mark = (item, announce = false) => {
        if (!item) return;
        document.querySelectorAll('.file-item.keyboard-selected').forEach(node => {
            node.classList.remove('keyboard-selected');
            node.removeAttribute('aria-selected');
        });
        item.classList.add('keyboard-selected');
        item.setAttribute('aria-selected', 'true');
        item.focus({preventScroll: true});
        item.scrollIntoView({block: 'nearest', inline: 'nearest'});
        // Keep keyboard selection on the same event path as pointer selection.
        // The listing preview and position restore code subscribe to this event,
        // so moving focus updates both the right-hand preview and saved state.
        if (announce) item.dispatchEvent(new CustomEvent('file:keyboard-selected', {
            bubbles: true,
            detail: {item},
        }));
    };
    const openItem = item => {
        const link = item?.querySelector('.favorite-file-link');
        if (!link) return;
        window.location.href = link.href;
    };
    const columns = items => {
        if (!items.length || items[0].parentElement?.id !== 'fileContainer') return 1;
        const firstTop = items[0].getBoundingClientRect().top;
        const count = items.filter(item => Math.abs(item.getBoundingClientRect().top - firstTop) < 2).length;
        return Math.max(1, count);
    };
    function prepare() {
        visibleItems().forEach(item => {
            if (!item.hasAttribute('tabindex')) item.tabIndex = 0;
            if (!item.hasAttribute('role')) item.setAttribute('role', 'gridcell');
        });
    }
    document.addEventListener('DOMContentLoaded', prepare, {once: true});
    document.addEventListener('files:appended', prepare);
    document.addEventListener('click', event => {
        const item = event.target.closest?.('.file-item');
        if (item && !isTyping(event.target)) mark(item);
    }, true);
    document.addEventListener('keydown', event => {
        if (isTyping(event.target) || event.defaultPrevented) return;
        const items = visibleItems();
        if (!items.length) return;
        const current = event.target.closest?.('.file-item') || document.querySelector('.file-item.keyboard-selected');
        if (!current) return;
        const index = items.indexOf(current);
        if (index < 0) return;
        let next = index;
        const cols = columns(items);
        switch (event.key) {
        case 'ArrowRight': next = Math.min(items.length - 1, index + 1); break;
        case 'ArrowLeft': next = Math.max(0, index - 1); break;
        case 'ArrowDown': next = Math.min(items.length - 1, index + cols); break;
        case 'ArrowUp': next = Math.max(0, index - cols); break;
        case 'Home': next = event.ctrlKey ? 0 : (Math.floor(index / cols) * cols); break;
        case 'End': next = event.ctrlKey ? items.length - 1 : Math.min(items.length - 1, Math.floor(index / cols) * cols + cols - 1); break;
        case 'Enter': openItem(current); event.preventDefault(); return;
        case ' ':
            current.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
            event.preventDefault();
            return;
        default: return;
        }
        if (next !== index) {
            mark(items[next], true);
            event.preventDefault();
        }
    });
})();
