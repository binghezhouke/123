(() => {
    const button = document.getElementById('archive-probe');
    if (!button) return;
    const status = document.getElementById('archive-probe-status');
    const open = document.getElementById('archive-probe-open');
    let pending = null;
    button.addEventListener('click', async () => {
        if (pending) return;
        const controller = new AbortController();
        pending = controller;
        button.disabled = true;
        button.textContent = '正在探测…';
        open.hidden = true;
        open.removeAttribute('href');
        status.hidden = false;
        status.textContent = '正在读取文件头…';
        try {
            const response = await fetch(button.dataset.url, {method: 'POST',
                headers: {'Content-Type': 'application/json'}, body: '{}', signal: controller.signal});
            const data = await response.json();
            if (!response.ok) throw new Error(data.error || '探测失败，请重试');
            status.textContent = data.message;
            if (data.browse_url) {
                open.href = data.browse_url;
                open.hidden = false;
            }
        } catch (error) {
            if (error.name !== 'AbortError') status.textContent = error.message || '探测失败，请重试';
        } finally {
            pending = null;
            button.disabled = false;
            button.textContent = '重新探测压缩格式';
        }
    });
    window.addEventListener('pagehide', () => pending?.abort());
})();
