(() => {
    const buttons = Array.from(document.querySelectorAll('.zip-client-video'));
    const dialog = document.getElementById('zip-client-video');
    if (!dialog || !buttons.length) return;
    const byId = name => document.getElementById(`zip-client-video-${name}`);
    const player = byId('player');
    const status = byId('status');
    const progress = byId('progress');
    let active = null;
    let cache = null;
    let generation = 0;
    let selectedButton = null;

    function releaseCache() {
        if (cache) URL.revokeObjectURL(cache.objectUrl);
        cache = null;
        byId('download').removeAttribute('href');
        byId('download').hidden = true;
    }
    function stopActive() {
        if (!active) return;
        active.controller?.abort();
        active.worker?.terminate();
        active = null;
    }
    function detachPlayer() {
        player.pause();
        player.removeAttribute('src');
        player.load();
    }
    function clearProgress() {
        progress.value = 0;
        progress.max = 1;
    }
    function setFailure(message) {
        status.textContent = message;
        byId('cancel').hidden = true;
        byId('retry').hidden = false;
    }
    function showCached(item) {
        player.src = cache.objectUrl;
        player.load();
        const link = byId('download');
        link.href = cache.objectUrl;
        link.download = item.filename || item.name;
        link.hidden = false;
        byId('cancel').hidden = true;
        byId('retry').hidden = true;
        status.textContent = '视频已准备完成，可以播放或拖动进度。';
        progress.max = 1;
        progress.value = 1;
    }
    async function prepare(button, token) {
        const url = button.dataset.url;
        const task = {token, controller: new AbortController(), worker: null};
        active = task;
        byId('cancel').hidden = false;
        byId('retry').hidden = true;
        byId('download').hidden = true;
        clearProgress();
        status.textContent = '正在读取视频信息…';
        try {
            const response = await fetch(url, {signal: task.controller.signal, headers: {Accept: 'application/json'}});
            if (token !== generation || active !== task) return;
            if (!response.ok) throw new Error(`视频信息读取失败（HTTP ${response.status}）`);
            const info = await response.json();
            if (token !== generation || active !== task) return;
            if (!info || typeof info.key !== 'string' || !info.key || typeof info.raw_url !== 'string' ||
                !Number.isFinite(info.size) || !Number.isFinite(info.compressed_size)) {
                throw new Error('服务器返回的视频信息无效');
            }
            if (typeof Worker !== 'function') throw new Error('此浏览器不支持后台解压 Worker');
            let worker;
            try { worker = new Worker(dialog.dataset.workerUrl); }
            catch (_) { throw new Error('无法启动视频解压 Worker'); }
            task.worker = worker;
            status.textContent = '正在下载并校验视频…';
            worker.onmessage = event => {
                if (token !== generation || active !== task) return;
                const message = event.data || {};
                if (message.type === 'progress') {
                    const total = Number(message.size || info.size);
                    progress.max = total > 0 ? total : 1;
                    progress.value = Math.min(Number(message.produced) || 0, progress.max);
                    const received = Number(message.received) || 0;
                    status.textContent = `正在下载并校验视频… ${formatBytes(received)} / ${formatBytes(Number(info.compressed_size) || 0)}，已解压 ${formatBytes(Number(message.produced) || 0)} / ${formatBytes(Number(info.size) || 0)}`;
                } else if (message.type === 'done') {
                    if (!(message.blob instanceof Blob) || message.blob.size !== info.size || message.blob.size > 64 * 1024 * 1024) {
                        fail(new Error('解压 Worker 未返回有效视频数据'));
                        return;
                    }
                    const objectUrl = URL.createObjectURL(message.blob);
                    cache = {key: info.key, urlKey: button.dataset.url, objectUrl};
                    active = null;
                    worker.terminate();
                    showCached({...info, name: button.dataset.name});
                } else if (message.type === 'error') {
                    fail(new Error(message.message || '视频解压或校验失败'));
                }
            };
            worker.onerror = () => fail(new Error('视频解压组件运行失败，请刷新页面或更换浏览器重试'));
            worker.postMessage({type: 'start', url: info.raw_url, size: info.size,
                compressedSize: info.compressed_size, crc32: info.crc32, mimetype: info.mimetype});
            function fail(error) {
                if (token !== generation || active !== task) return;
                stopActive();
                setFailure(error.message || '视频处理失败');
            }
        } catch (error) {
            if (token !== generation || active !== task || error.name === 'AbortError') return;
            stopActive();
            setFailure(error.message || '视频读取失败');
        }
    }
    function formatBytes(value) {
        if (!Number.isFinite(value) || value <= 0) return '0 B';
        if (value < 1024) return `${Math.floor(value)} B`;
        const units = ['KiB', 'MiB', 'GiB'];
        let amount = value / 1024, unit = 0;
        while (amount >= 1024 && unit < units.length - 1) { amount /= 1024; unit++; }
        return `${amount.toFixed(1)} ${units[unit]}`;
    }
    function open(button) {
        const keyHint = button.dataset.url;
        selectedButton = button;
        const token = ++generation;
        stopActive();
        detachPlayer();
        if (!cache || cache.urlKey !== keyHint) releaseCache();
        clearProgress();
        status.textContent = '首次准备完成后播放，当前页面保留一个视频。';
        byId('retry').hidden = true;
        byId('cancel').hidden = true;
        const gallery = document.getElementById('zip-gallery');
        if (gallery?.open) gallery.close();
        byId('title').textContent = button.dataset.name;
        if (!dialog.open) dialog.showModal();
        if (cache?.urlKey === keyHint) {
            showCached({filename: button.dataset.name, name: button.dataset.name});
            return;
        }
        prepare(button, token);
    }
    buttons.forEach(button => button.addEventListener('click', () => {
        if (active && dialog.open && active.url === button.dataset.url) return;
        open(button);
        if (active) active.url = button.dataset.url;
    }));
    player.addEventListener('error', () => {
        if (!cache || !dialog.open || player.src !== cache.objectUrl) return;
        status.textContent = '准备已完成，但浏览器不支持此视频编码或视频损坏';
        byId('cancel').hidden = true;
        byId('retry').hidden = false;
        byId('download').hidden = false;
    });
    byId('cancel').addEventListener('click', () => {
        ++generation;
        stopActive();
        clearProgress();
        status.textContent = '已取消。';
        byId('cancel').hidden = true;
        byId('retry').hidden = false;
    });
    byId('retry').addEventListener('click', () => {
        const selected = selectedButton;
        if (!selected) return;
        ++generation;
        stopActive();
        detachPlayer();
        releaseCache();
        prepare(selected, generation);
    });
    byId('close').addEventListener('click', () => dialog.close());
    dialog.addEventListener('close', () => {
        if (dialog.open) return;
        ++generation;
        stopActive();
        detachPlayer();
        clearProgress();
        byId('cancel').hidden = true;
    });
    dialog.addEventListener('cancel', () => {
        ++generation;
        stopActive();
    });
    window.addEventListener('pagehide', () => {
        ++generation;
        stopActive();
        detachPlayer();
        releaseCache();
    });
})();
