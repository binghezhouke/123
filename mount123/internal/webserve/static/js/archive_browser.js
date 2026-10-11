(() => {
    const root = document.querySelector('.browse-files');
    const pane = document.getElementById('preview-pane');
    if (!root || !pane) return;
    const stage = document.getElementById('preview-stage');
    const title = document.getElementById('preview-title');
    const meta = document.getElementById('preview-meta');
    const actions = document.getElementById('preview-actions');
    const empty = pane.querySelector('.preview-empty');
    const open = document.getElementById('preview-open');
    const download = document.getElementById('preview-download');
    const selected = () => document.querySelector('.file-item.is-selected');
    const typeOf = name => {
        const ext = (name.split('.').pop() || '').toLowerCase();
        if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp'].includes(ext)) return 'image';
        if (['mp4', 'm4v', 'webm', 'mov', 'mkv'].includes(ext)) return 'video';
        if (['txt', 'md', 'log', 'json', 'csv'].includes(ext)) return 'text';
        return 'file';
    };
    const items = () => [...document.querySelectorAll('.file-item')].filter(item => {
        const view = item.closest('#fileContainer, #fileListView');
        return !view || getComputedStyle(view).display !== 'none';
    });
    const icon = (name, color) => {
        const node = document.createElement('i');
        node.className = name;
        node.style.cssText = `font-size:64px;color:${color};`;
        return node;
    };
    function show(item) {
        if (!item) return;
        document.querySelectorAll('.file-item.is-selected').forEach(node => node.classList.remove('is-selected'));
        item.classList.add('is-selected');
        const name = item.dataset.favoriteName || '';
        const kind = typeOf(name);
        const isFolder = item.dataset.fileType === 'folder';
        const url = item.dataset.url || item.dataset.favoriteUrl || '#';
        empty.hidden = true; title.hidden = false; title.textContent = name;
        stage.hidden = false; stage.replaceChildren(); actions.hidden = false; open.href = item.dataset.favoriteUrl || url;
        if (download) { download.href = item.dataset.download || url; download.hidden = isFolder || !item.dataset.download; }
        meta.hidden = false;
        meta.innerHTML = `<div>类型：${item.dataset.kind || (isFolder ? '文件夹' : '文件')}</div><div>大小：${isFolder ? '—' : (item.dataset.size || '—')}</div>`;
        if (isFolder) {
            stage.append(icon(item.dataset.kind === '压缩包' ? 'fas fa-file-archive' : 'fas fa-folder', '#76a9e6'));
            return;
        }
        if (kind === 'image') {
            const image = document.createElement('img'); image.alt = name; image.src = url;
            image.onload = () => { meta.innerHTML += `<div>分辨率：${image.naturalWidth} × ${image.naturalHeight}</div>`; };
            stage.append(image);
        } else if (kind === 'video') {
            const video = document.createElement('video'); video.src = url; video.controls = true; video.preload = 'metadata'; stage.append(video);
        } else if (kind === 'text') {
            const pre = document.createElement('pre'); pre.textContent = '正在读取…';
            fetch(url).then(response => response.ok ? response.text() : Promise.reject(new Error('读取失败'))).then(text => { pre.textContent = text.slice(0, 20000); }).catch(() => { pre.textContent = '无法预览此文件'; });
            stage.append(pre);
        } else stage.append(icon('fas fa-file', '#8a93a1'));
    }
    function switchView(view) {
        const grid = document.getElementById('fileContainer'), list = document.getElementById('fileListView');
        if (grid) grid.style.display = view === 'list' ? 'none' : 'grid';
        if (list) list.style.display = view === 'list' ? 'block' : 'none';
        try { localStorage.setItem('finderViewMode', view); } catch {}
    }
    globalThis.switchView = switchView;
    try { if (localStorage.getItem('finderViewMode') === 'list') switchView('list'); } catch {}
    const manager = globalThis.CloudViewportImages && new CloudViewportImages(root, document.getElementById('preview-debug-output'));
    document.getElementById('preview-debug-toggle')?.addEventListener('click', event => {
        const output = document.getElementById('preview-debug-output'); if (!output) return;
        const openState = output.hidden; output.hidden = !openState; event.currentTarget.setAttribute('aria-expanded', String(openState)); if (openState) manager?.render();
    });
    document.addEventListener('click', event => {
        const item = event.target.closest('.file-item');
        if (!item || event.target.closest('.favorite-marker')) return;
        show(item);
        if (item.dataset.fileType !== 'folder') event.preventDefault();
    }, true);
    document.addEventListener('keydown', event => {
        if (event.target.matches('input, textarea, select')) return;
        const list = items(); if (!list.length) return;
        const current = selected(), index = current ? list.indexOf(current) : -1;
        let next = index;
        if (event.key === 'ArrowRight' || event.key === 'ArrowDown') next = Math.min(list.length - 1, Math.max(0, index) + 1);
        if (event.key === 'ArrowLeft' || event.key === 'ArrowUp') next = Math.max(0, (index < 0 ? 0 : index) - 1);
        if (next !== index && next >= 0) { event.preventDefault(); show(list[next]); list[next].scrollIntoView({block: 'nearest'}); }
    });
    document.getElementById('preview-fullscreen')?.addEventListener('click', () => {
        if (document.fullscreenElement) document.exitFullscreen(); else pane.requestFullscreen?.();
    });
    const resizer = document.getElementById('preview-resizer');
    if (resizer) {
        let dragging = false;
        resizer.addEventListener('mousedown', event => { dragging = true; event.preventDefault(); });
        document.addEventListener('mousemove', event => { if (!dragging) return; const right = pane.parentElement.getBoundingClientRect().right; const width = Math.max(200, Math.min(640, right - event.clientX)); pane.style.flex = `0 0 ${width}px`; pane.style.maxWidth = 'none'; });
        document.addEventListener('mouseup', () => { dragging = false; });
    }
})();
