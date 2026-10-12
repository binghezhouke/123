// Keep a short, local list of recently visited browser locations.
(() => {
    const list = document.getElementById('sidebar-history-list');
    const clear = document.getElementById('sidebar-history-clear');
    if (!list || !clear) return;

    const KEY = 'cloudarchive.history.v1';
    const MAX_ITEMS = 8;

    function locationURL(value) {
        try {
            const url = new URL(value, location.href);
            if (url.origin !== location.origin || url.username || url.password ||
                !(url.pathname === '/browse' || /^\/archive\/[1-9]\d*$/.test(url.pathname))) return null;
            const allowed = url.pathname === '/browse' ? ['parent_id'] : ['path'];
            for (const name of [...url.searchParams.keys()]) {
                if (!allowed.includes(name)) url.searchParams.delete(name);
            }
            if (url.pathname === '/browse' && !url.searchParams.has('parent_id')) url.searchParams.set('parent_id', '0');
            url.hash = '';
            url.searchParams.sort();
            return url;
        } catch (_) {
            return null;
        }
    }

    function read() {
        try {
            const value = JSON.parse(localStorage.getItem(KEY) || '[]');
            if (!Array.isArray(value)) return [];
            return value.filter(item => item && typeof item.url === 'string' && typeof item.name === 'string' && item.name.length <= 4096 && locationURL(item.url))
                .slice(0, MAX_ITEMS);
        } catch (_) {
            return [];
        }
    }

    function write(value) {
        try { localStorage.setItem(KEY, JSON.stringify(value.slice(0, MAX_ITEMS))); } catch (_) { /* Storage may be disabled. */ }
    }

    function currentName() {
        const heading = document.querySelector('.toolbar-directory-title h1, .directory-heading h1');
        const value = heading?.textContent?.trim();
        if (value) return value;
        return document.title.replace(/\s*·\s*CloudArchive\s*$/, '').trim() || '未命名目录';
    }

    function iconFor(url) {
        return url.pathname.startsWith('/archive/') ? 'fas fa-file-archive' : 'fas fa-folder';
    }

    function render() {
        const entries = read();
        list.replaceChildren();
        if (!entries.length) {
            const empty = document.createElement('div');
            empty.className = 'sidebar-history-empty';
            empty.textContent = '暂无历史记录';
            list.append(empty);
            clear.disabled = true;
            return;
        }
        clear.disabled = false;
        for (const entry of entries) {
            const url = locationURL(entry.url);
            if (!url) continue;
            const link = document.createElement('a');
            link.className = 'sidebar-item sidebar-history-item';
            link.href = url.href;
            link.title = entry.name;
            const icon = document.createElement('span');
            icon.className = 'sidebar-icon';
            icon.innerHTML = `<i class="${iconFor(url)}" aria-hidden="true"></i>`;
            const name = document.createElement('span');
            name.className = 'sidebar-history-name';
            name.textContent = entry.name.split('/').filter(Boolean).pop() || entry.name;
            link.append(icon, name);
            list.append(link);
        }
    }

    function record(value) {
        const url = locationURL(value);
        if (!url) return;
        const identity = url.href;
        const entries = read().filter(entry => entry.url !== identity);
        entries.unshift({url: identity, name: currentName(), at: Date.now()});
        write(entries);
        render();
    }

    clear.addEventListener('click', () => {
        try { localStorage.removeItem(KEY); } catch (_) {}
        render();
    });
    window.addEventListener('storage', event => { if (event.key === KEY || event.key === null) render(); });
    document.addEventListener('cloudarchive:navigated', event => record(event.detail?.url || location.href));
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', () => { record(location.href); render(); }, {once: true});
    } else {
        record(location.href);
        render();
    }
})();
