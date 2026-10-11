(() => {
    const KEY = 'cloudarchive.favorites.v1';
    const MAX_ITEMS = 5000;
    const MAX_IMPORT_BYTES = 1024 * 1024;
    const colors = {red: ['红色', '#e85d59'], orange: ['橙色', '#ec983d'], yellow: ['黄色', '#d9b634'],
        green: ['绿色', '#58a970'], blue: ['蓝色', '#508bd9'], purple: ['紫色', '#a377c9'], gray: ['灰色', '#8b919a']};
    const picker = document.getElementById('favorite-picker');
    if (!picker) return;
    let selected = null;
    let filter = new URL(location.href).searchParams.get('color') || '';
    if (!Object.hasOwn(colors, filter)) filter = '';

    function safeURL(value) {
        if (typeof value !== 'string' || value.length > 4096) return false;
        try {
            const url = new URL(value, location.href);
            if (url.origin !== location.origin || !/^https?:$/.test(url.protocol) || url.username || url.password) return false;
            for (const name of url.searchParams.keys()) if (/(token|password|secret|auth|credential|signature)/i.test(name)) return false;
            return true;
        } catch { return false; }
    }
    function validKey(key) {
        return typeof key === 'string' && (/^file:\d+$/.test(key) || /^zip:\d+:[^:]{1,512}:\d+$/.test(key));
    }
    function validRecord(key, item) {
        return validKey(key) && item && typeof item === 'object' && !Array.isArray(item) &&
            Object.hasOwn(colors, item.color) && typeof item.name === 'string' && item.name.length <= 2048 &&
            safeURL(item.url) && (item.memberPath === undefined ||
                (typeof item.memberPath === 'string' && item.memberPath.length <= 4096 && !item.memberPath.startsWith('/')));
    }
    function read() {
        try {
            const raw = JSON.parse(localStorage.getItem(KEY) || '{}');
            if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
            return Object.fromEntries(Object.entries(raw).slice(0, MAX_ITEMS).filter(([key, item]) => validRecord(key, item)));
        } catch { return {}; }
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
            const key = item.dataset.favoriteKey;
            if (!validKey(key)) return;
            let button = item.querySelector('.favorite-marker');
            if (!button) {
                button = document.createElement('button');
                button.type = 'button';
                button.className = 'favorite-marker';
                button.setAttribute('aria-haspopup', 'dialog');
                (item.tagName === 'TR' ? item.cells[0] : item).append(button);
            }
            colorize(button, records[key]?.color);
        });
    }
    function save(color) {
        if (!selected) return;
        const next = read();
        const memberPath = selected.memberPath ?? next[selected.key]?.memberPath;
        if (color) next[selected.key] = {name: selected.name, url: selected.url, color, ...(memberPath !== undefined ? {memberPath} : {})};
        else delete next[selected.key];
        try {
            if (Object.keys(next).length > MAX_ITEMS) throw new Error('limit');
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
        button.type = 'button'; button.className = 'favorite-color-choice'; button.dataset.color = color;
        button.style.setProperty('--favorite-color', hex); button.textContent = label;
        button.addEventListener('click', () => save(color)); palette.append(button);
    }
    document.getElementById('favorite-remove').addEventListener('click', () => save(null));
    document.addEventListener('click', event => {
        const button = event.target.closest('.favorite-marker');
        if (!button) return;
        event.preventDefault(); event.stopPropagation();
        const item = button.closest('[data-favorite-key]');
        const candidate = {key: item.dataset.favoriteKey, name: item.dataset.favoriteName, url: item.dataset.favoriteUrl, memberPath: item.dataset.favoriteMemberPath};
        if (!validRecord(candidate.key, {...candidate, color: records[candidate.key]?.color || 'gray'})) return;
        selected = candidate;
        document.getElementById('favorite-picker-title').textContent = selected.name;
        document.getElementById('favorite-message').textContent = '';
        const color = records[selected.key]?.color;
        document.getElementById('favorite-remove').hidden = !color;
        palette.querySelectorAll('button').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.color === color)));
        picker.showModal();
    });

    function openURL(key, item) {
        const url = new URL(item.url, location.href);
        const match = /^zip:(\d+):([^:]+):(\d+)$/.exec(key);
        if (!match) return url.href;
        // The version query is checked by the archive route before it resolves the member index.
        const prefix = url.pathname.match(/^(.*)\/file\/\d+\/zip(?:\/member\/\d+)?$/)?.[1] || '';
        url.pathname = `${prefix}/file/${match[1]}/zip/member/${match[3]}`;
        url.search = '';
        url.searchParams.set('v', match[2]);
        if (item.memberPath) url.searchParams.set('expected_path', item.memberPath);
        url.hash = '';
        return url.href;
    }
    function render() {
        const container = document.getElementById('favorite-results');
        if (!container) return;
        container.replaceChildren();
        const items = Object.entries(records).filter(([, item]) => !filter || item.color === filter);
        for (const [key, item] of items) {
            const card = document.createElement('div'); card.className = 'file-grid-item';
            Object.assign(card.dataset, {favoriteKey: key, favoriteName: item.name, favoriteUrl: item.url,
                ...(item.memberPath !== undefined ? {favoriteMemberPath: item.memberPath} : {})});
            const link = document.createElement('a'); link.className = 'favorite-file-link'; link.href = openURL(key, item);
            const icon = document.createElement('span'); icon.className = 'favorite-large-dot';
            icon.style.setProperty('--favorite-color', colors[item.color][1]); icon.textContent = '●'; icon.setAttribute('aria-hidden', 'true');
            const name = document.createElement('div'); name.className = 'file-grid-name'; name.textContent = item.name;
            const label = document.createElement('span'); label.className = 'file-grid-meta';
            label.textContent = `${colors[item.color][0]} · ${key.startsWith('zip:') ? '压缩包内文件' : '文件或文件夹'}`;
            link.append(icon, name, label); card.append(link); container.append(card);
        }
        document.getElementById('favorite-count').textContent = `${items.length} 个收藏`;
        const empty = document.getElementById('favorite-empty'); empty.hidden = items.length > 0;
        empty.textContent = filter ? '这个颜色还没有收藏。' : '还没有收藏。在文件旁点击 ☆，选择一种颜色即可收藏。';
        document.querySelectorAll('[data-favorite-filter]').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.favoriteFilter === filter)));
    }
    const filters = document.getElementById('favorite-filters');
    if (filters) for (const [color, [label, hex]] of [['', ['全部', '#87909d']], ...Object.entries(colors)]) {
        const button = document.createElement('button'); button.type = 'button'; button.className = 'favorite-filter';
        button.dataset.favoriteFilter = color; button.style.setProperty('--favorite-color', hex); button.textContent = label;
        button.addEventListener('click', () => {
            filter = color; const url = new URL(location.href);
            if (color) url.searchParams.set('color', color); else url.searchParams.delete('color');
            history.replaceState(null, '', url); render(); decorate();
        });
        filters.append(button);
    }

    const importInput = document.getElementById('favorite-import-file');
    const transferMessage = document.getElementById('favorite-transfer-message');
    document.getElementById('favorite-export')?.addEventListener('click', () => {
        const favorites = Object.entries(records).filter(([key, item]) => validRecord(key, item)).map(([identity, item]) => ({
            identity, name: item.name, url: item.url, color: item.color, ...(item.memberPath ? {memberPath: item.memberPath} : {}),
        }));
        const blob = new Blob([JSON.stringify({version: 1, favorites}, null, 2)], {type: 'application/json'});
        const url = URL.createObjectURL(blob); const anchor = document.createElement('a');
        anchor.href = url; anchor.download = 'cloudarchive-favorites.json'; anchor.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
        transferMessage.textContent = `已导出 ${favorites.length} 个收藏。`;
    });
    importInput?.addEventListener('change', async () => {
        const file = importInput.files?.[0]; importInput.value = '';
        if (!file) return;
        if (file.size > MAX_IMPORT_BYTES) { transferMessage.textContent = '导入文件超过 1 MB，现有收藏未更改。'; return; }
        let data;
        try { data = JSON.parse(await file.text()); }
        catch { transferMessage.textContent = '文件不是有效 JSON，现有收藏未更改。'; return; }
        let entries;
        if (data && data.version === 1 && Array.isArray(data.favorites)) {
            entries = data.favorites.map(item => [item?.identity, item]);
        } else if (data && typeof data === 'object' && !Array.isArray(data)) {
            // Accept the original localStorage object format for backup compatibility.
            entries = Object.entries(data);
        } else { transferMessage.textContent = '收藏文件格式无法识别，现有收藏未更改。'; return; }
        if (entries.length > MAX_ITEMS) { transferMessage.textContent = `收藏数量超过 ${MAX_ITEMS} 个，现有收藏未更改。`; return; }
        const next = read(); let added = 0, duplicate = 0, invalid = 0;
        for (const [identity, raw] of entries) {
            const item = raw && typeof raw === 'object' ? {name: raw.name, url: raw.url, color: raw.color, ...(raw.memberPath ? {memberPath: raw.memberPath} : {})} : null;
            if (!validRecord(identity, item)) { invalid++; continue; }
            if (Object.hasOwn(next, identity)) { duplicate++; continue; }
            if (Object.keys(next).length >= MAX_ITEMS) { invalid++; continue; }
            next[identity] = item; added++;
        }
        if (added) {
            try { localStorage.setItem(KEY, JSON.stringify(next)); }
            catch { transferMessage.textContent = '浏览器存储不可用或已满，现有收藏未更改。'; return; }
            records = next; render(); decorate();
        }
        transferMessage.textContent = `导入完成：新增 ${added} 个，重复 ${duplicate} 个，无效 ${invalid} 个。`;
    });

    window.addEventListener('storage', event => { if (event.key === KEY || event.key === null) { records = read(); render(); decorate(); } });
    for (const id of ['fileContainer', 'fileListView']) {
        const container = document.getElementById(id);
        if (container) new MutationObserver(() => decorate(container)).observe(container, {childList: true});
    }
    render(); decorate();
})();
