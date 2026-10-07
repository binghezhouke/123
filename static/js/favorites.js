(() => {
    const KEY = 'cloudarchive.favorites.v1';
    const colors = {red: ['红色', '#e85d59'], orange: ['橙色', '#ec983d'], yellow: ['黄色', '#d9b634'],
        green: ['绿色', '#58a970'], blue: ['蓝色', '#508bd9'], purple: ['紫色', '#a377c9'], gray: ['灰色', '#8b919a']};
    const picker = document.getElementById('favorite-picker');
    if (!picker) return;
    let selected = null;
    let filter = new URL(location.href).searchParams.get('color') || '';
    if (!Object.hasOwn(colors, filter)) filter = '';
    function read() {
        try {
            const raw = JSON.parse(localStorage.getItem(KEY) || '{}');
            if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
            return Object.fromEntries(Object.entries(raw).slice(0, 5000).filter(([key, item]) => {
                if (!key.startsWith('file:') && !key.startsWith('zip:')) return false;
                if (!item || !Object.hasOwn(colors, item.color) || typeof item.name !== 'string' || typeof item.url !== 'string') return false;
                const url = new URL(item.url, location.href);
                return url.origin === location.origin && /^https?:$/.test(url.protocol);
            }));
        } catch {return {};}
    }
    let records = read();
    function colorize(button, color) {
        button.style.setProperty('--favorite-color', colors[color]?.[1] || '#87909d');
        button.textContent = color ? '●' : '☆';
        button.setAttribute('aria-label', color ? `${colors[color][0]}收藏，修改标记` : '收藏文件');
        button.title = color ? `${colors[color][0]}收藏` : '收藏文件';
        button.classList.toggle('is-favorite', !!color);
    }
    function decorate(root = document) {
        root.querySelectorAll('[data-favorite-key]').forEach(item => {
            let button = item.querySelector('.favorite-marker');
            if (!button) {
                button = document.createElement('button');
                button.type = 'button';
                button.className = 'favorite-marker';
                button.setAttribute('aria-haspopup', 'dialog');
                (item.tagName === 'TR' ? item.cells[0] : item).append(button);
            }
            colorize(button, records[item.dataset.favoriteKey]?.color);
        });
    }
    function save(color) {
        if (!selected) return;
        const next = read();
        if (color) next[selected.key] = {name: selected.name, url: selected.url, color};
        else delete next[selected.key];
        try {
            if (Object.keys(next).length > 5000) throw new Error('limit');
            localStorage.setItem(KEY, JSON.stringify(next));
        } catch {
            document.getElementById('favorite-message').textContent = '无法保存收藏：浏览器存储不可用或已满，请释放空间后重试。';
            return;
        }
        records = next;
        picker.close();
        render();
        decorate();
    }
    const palette = document.getElementById('favorite-colors');
    for (const [color, [label, hex]] of Object.entries(colors)) {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'favorite-color-choice';
        button.dataset.color = color;
        button.style.setProperty('--favorite-color', hex);
        button.textContent = label;
        button.addEventListener('click', () => save(color));
        palette.append(button);
    }
    document.getElementById('favorite-remove').addEventListener('click', () => save(null));
    document.addEventListener('click', event => {
        const button = event.target.closest('.favorite-marker');
        if (!button) return;
        event.preventDefault(); event.stopPropagation();
        const item = button.closest('[data-favorite-key]');
        selected = {key: item.dataset.favoriteKey, name: item.dataset.favoriteName, url: item.dataset.favoriteUrl};
        document.getElementById('favorite-picker-title').textContent = selected.name;
        document.getElementById('favorite-message').textContent = '';
        const color = records[selected.key]?.color;
        document.getElementById('favorite-remove').hidden = !color;
        palette.querySelectorAll('button').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.color === color)));
        picker.showModal();
    });
    function render() {
        const container = document.getElementById('favorite-results');
        if (!container) return;
        container.replaceChildren();
        const items = Object.entries(records).filter(([, item]) => !filter || item.color === filter);
        for (const [key, item] of items) {
            const card = document.createElement('div');
            card.className = 'file-grid-item';
            Object.assign(card.dataset, {favoriteKey: key, favoriteName: item.name, favoriteUrl: item.url});
            const link = document.createElement('a');
            link.className = 'favorite-file-link';
            link.href = item.url;
            const icon = document.createElement('span');
            icon.className = 'favorite-large-dot';
            icon.style.setProperty('--favorite-color', colors[item.color][1]);
            icon.textContent = '●'; icon.setAttribute('aria-hidden', 'true');
            const name = document.createElement('div');
            name.className = 'file-grid-name'; name.textContent = item.name;
            const label = document.createElement('span');
            label.className = 'file-grid-meta'; label.textContent = `${colors[item.color][0]} · ${key.startsWith('zip:') ? '压缩包内文件' : '文件或文件夹'}`;
            link.append(icon, name, label); card.append(link); container.append(card);
        }
        document.getElementById('favorite-count').textContent = `${items.length} 个收藏`;
        const empty = document.getElementById('favorite-empty');
        empty.hidden = items.length > 0;
        empty.textContent = filter ? '这个颜色还没有收藏。' : '还没有收藏。在文件旁点击 ☆，选择一种颜色即可收藏。';
        document.querySelectorAll('[data-favorite-filter]').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.favoriteFilter === filter)));
    }
    const filters = document.getElementById('favorite-filters');
    if (filters) for (const [color, [label, hex]] of [['', ['全部', '#87909d']], ...Object.entries(colors)]) {
        const button = document.createElement('button');
        button.type = 'button'; button.className = 'favorite-filter'; button.dataset.favoriteFilter = color;
        button.style.setProperty('--favorite-color', hex); button.textContent = label;
        button.addEventListener('click', () => {
            filter = color;
            const url = new URL(location.href);
            if (color) url.searchParams.set('color', color); else url.searchParams.delete('color');
            history.replaceState(null, '', url);
            render(); decorate();
        });
        filters.append(button);
    }
    window.addEventListener('storage', event => {if (event.key === KEY || event.key === null) {records = read(); render(); decorate();}});
    // Newly appended pagination entries inherit their saved marker without a reload.
    for (const id of ['fileContainer', 'fileListView']) {
        const container = document.getElementById(id);
        if (container) new MutationObserver(() => decorate(container)).observe(container, {childList: true});
    }
    render(); decorate();
})();
