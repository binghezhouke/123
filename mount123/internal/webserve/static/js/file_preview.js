// Shared right-hand preview for directory and archive file lists.
(() => {
    const mediaType = name => {
        const ext = (String(name || '').split('.').pop() || '').toLowerCase();
        if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp'].includes(ext)) return 'image';
        if (['mp4', 'm4v', 'webm', 'mov', 'mkv'].includes(ext)) return 'video';
        if (['txt', 'md', 'log', 'json', 'csv'].includes(ext)) return 'text';
        return 'file';
    };
    const itemIdentity = item => item?.dataset.favoriteUrl || item?.dataset.url || '';
    const remuxURL = (url, base) => {
        try {
            const target = new URL(url, base);
            target.searchParams.set('transcode', 'mp4');
            return target.href;
        } catch (_) { return `${url}${String(url).includes('?') ? '&' : '?'}transcode=mp4`; }
    };
    const createGeneration = () => {
        let value = 0;
        let controller = null;
        return {
            next() { controller?.abort(); controller = new AbortController(); return {token: ++value, signal: controller.signal}; },
            current(token) { return token === value; },
            cancel() { controller?.abort(); controller = null; value++; },
        };
    };
    const api = {mediaType, itemIdentity, remuxURL, createGeneration};
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof document === 'undefined' || globalThis.__cloudFilePreviewInstalled) return;
    globalThis.__cloudFilePreviewInstalled = true;

    const generation = createGeneration();
    let previewObjectURL = null;
    const viewportOutput = document.getElementById('preview-debug-output');
    const viewport = globalThis.CloudViewportImages && document.querySelector('.browse-files')
        ? new globalThis.CloudViewportImages(document.querySelector('.browse-files'), viewportOutput)
        : null;
    globalThis.cloudViewportImages = viewport;
    if (!globalThis.switchView) {
        globalThis.switchView = view => {
            const grid = document.getElementById('fileContainer');
            const list = document.getElementById('fileListView');
            const isList = view === 'list';
            if (grid) grid.style.display = isList ? 'none' : 'grid';
            if (list) list.style.display = isList ? 'block' : 'none';
            try { localStorage.setItem('finderViewMode', isList ? 'list' : 'icons'); } catch (_) {}
        };
    }
    const currentParts = () => {
        const pane = document.getElementById('preview-pane');
        return pane && {pane, stage: document.getElementById('preview-stage'), title: document.getElementById('preview-title'),
            meta: document.getElementById('preview-meta'), actions: document.getElementById('preview-actions'),
            empty: pane.querySelector('.preview-empty'), open: document.getElementById('preview-open'),
            download: document.getElementById('preview-download')};
    };
    const icon = (className, color) => {
        const node = document.createElement('i'); node.className = className;
        node.style.cssText = `font-size:64px;color:${color};`; return node;
    };
    const setMeta = (meta, values) => meta.replaceChildren(...values.map(value => {
        const line = document.createElement('div'); line.textContent = value; return line;
    }));
    const selectCopies = item => {
        const identity = itemIdentity(item);
        document.querySelectorAll('.file-item.is-selected, .file-item.keyboard-selected').forEach(node => {
            node.classList.remove('is-selected', 'keyboard-selected'); node.removeAttribute('aria-selected');
        });
        const copies = [...document.querySelectorAll('.file-item')].filter(node => identity && itemIdentity(node) === identity);
        (copies.length ? copies : [item]).forEach(node => {
            node.classList.add('is-selected', 'keyboard-selected'); node.setAttribute('aria-selected', 'true');
        });
    };
    const show = item => {
        const parts = currentParts();
        if (!parts || !item?.closest('.browse-files')) return;
        const {stage, title, meta, actions, empty, open, download} = parts;
        if (!stage || !title || !meta || !actions || !empty || !open) return;
        const request = generation.next();
        if (previewObjectURL) { URL.revokeObjectURL(previewObjectURL); previewObjectURL = null; }
        const name = item.dataset.favoriteName || item.dataset.name || '';
        const isFolder = item.dataset.fileType === 'folder';
        const kind = mediaType(name);
        const previewURL = item.dataset.url || item.dataset.favoriteUrl || '#';
        const openURL = item.dataset.favoriteUrl || item.dataset.url || '#';
        const metaValues = [`类型：${item.dataset.kind || (isFolder ? '文件夹' : '文件')}`, `大小：${isFolder ? '—' : (item.dataset.size || '—')}`];
        if (item.dataset.updated) metaValues.push(`修改时间：${item.dataset.updated}`);
        selectCopies(item);
        empty.hidden = true; title.hidden = false; title.textContent = name;
        stage.hidden = false; stage.replaceChildren(); meta.hidden = false; setMeta(meta, metaValues);
        actions.hidden = false; open.href = openURL;
        if (download) { download.href = item.dataset.download || previewURL; download.hidden = isFolder || !item.dataset.download; }
        if (isFolder) {
            stage.append(icon(item.dataset.kind === '压缩包' ? 'fas fa-file-archive' : 'fas fa-folder', '#76a9e6'));
        } else if (kind === 'image') {
            const image = document.createElement('img'); image.alt = name;
            const read = globalThis.imageReadCoordinator?.fetch;
            if (read) read(previewURL, {cache: 'force-cache', signal: request.signal}).then(response => response.blob()).then(blob => {
                if (!generation.current(request.token) || !image.isConnected) return;
                previewObjectURL = URL.createObjectURL(blob);
                image.src = previewObjectURL;
            }).catch(error => { if (error.name !== 'AbortError' && generation.current(request.token)) image.src = previewURL; });
            else image.src = previewURL;
            image.onload = () => {
                if (!generation.current(request.token) || !image.isConnected) return;
                if (image.naturalWidth && image.naturalHeight) setMeta(meta, [...metaValues, `分辨率：${image.naturalWidth} × ${image.naturalHeight}`]);
            };
            stage.append(image);
        } else if (kind === 'video') {
            const video = document.createElement('video'); video.controls = true; video.preload = 'metadata';
            const isMKV = /\.mkv$/i.test(name);
            const nativeMKV = typeof video.canPlayType === 'function' && video.canPlayType('video/x-matroska') !== '';
            video.src = isMKV && !nativeMKV ? remuxURL(previewURL, location.href) : previewURL;
            video.addEventListener('error', () => {
                if (!generation.current(request.token) || !video.isConnected || !isMKV || video.dataset.remuxed) return;
                video.dataset.remuxed = 'true'; video.src = remuxURL(previewURL, location.href); video.load();
            });
            stage.append(video);
        } else if (kind === 'text') {
            const pre = document.createElement('pre');
            pre.style.cssText = 'white-space:pre-wrap;font-size:12px;color:#4a5560;max-height:100%;overflow:auto;';
            pre.textContent = '正在读取…'; stage.append(pre);
            fetch(previewURL, {signal: request.signal})
                .then(response => response.ok ? response.text() : Promise.reject(Error(`HTTP ${response.status}`)))
                .then(text => { if (generation.current(request.token) && pre.isConnected) pre.textContent = text.slice(0, 20000); })
                .catch(error => { if (error.name !== 'AbortError' && generation.current(request.token) && pre.isConnected) pre.textContent = '无法预览此文件'; });
        } else stage.append(icon('fas fa-file', '#8a93a1'));
        globalThis.cloudViewportImages?.render?.();
    };
    document.addEventListener('click', event => {
        const item = event.target.closest?.('.file-item');
        if (!item?.closest('.browse-files') || event.target.closest?.('.favorite-marker')) return;
        show(item);
        if (item.dataset.fileType !== 'folder' && item.dataset.kind !== '压缩包') event.preventDefault();
    }, true);
    document.addEventListener('file:keyboard-selected', event => show(event.detail?.item || event.target.closest?.('.file-item')));
    document.addEventListener('dblclick', event => {
        const item = event.target.closest?.('.file-item');
        if (!item?.closest('.browse-files') || item.dataset.fileType === 'folder' || event.target.closest?.('.favorite-marker')) return;
        const url = item.dataset.favoriteUrl || item.dataset.url; if (url) location.href = url;
    });
    document.addEventListener('cloudarchive:navigated', () => {
        generation.cancel();
        if (previewObjectURL) { URL.revokeObjectURL(previewObjectURL); previewObjectURL = null; }
    });
    document.addEventListener('click', event => {
        if (!event.target.closest?.('#preview-fullscreen')) return;
        const pane = currentParts()?.pane;
        if (document.fullscreenElement) document.exitFullscreen(); else pane?.requestFullscreen?.();
    });
    let dragging = false;
    document.addEventListener('mousedown', event => {
        const resizer = event.target.closest?.('#preview-resizer'); if (!resizer) return;
        dragging = true; resizer.classList.add('dragging'); event.preventDefault();
    });
    document.addEventListener('mousemove', event => {
        if (!dragging) return;
        const pane = currentParts()?.pane; if (!pane?.parentElement) return;
        const width = Math.max(200, Math.min(640, pane.parentElement.getBoundingClientRect().right - event.clientX));
        pane.style.flex = `0 0 ${width}px`; pane.style.maxWidth = 'none';
    });
    document.addEventListener('mouseup', () => {
        if (!dragging) return; dragging = false; document.getElementById('preview-resizer')?.classList.remove('dragging');
    });
})();
