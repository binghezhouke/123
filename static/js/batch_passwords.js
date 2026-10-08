(() => {
    const form = document.getElementById('batch-password-form');
    if (!form) return;
    const input = document.getElementById('batch-password');
    const skipValidation = document.getElementById('batch-skip-validation');
    const start = document.getElementById('batch-start');
    const stop = document.getElementById('batch-stop');
    const status = document.getElementById('batch-status');
    const progress = document.getElementById('batch-progress');
    const results = document.getElementById('batch-results');
    let running = false;
    let stopping = false;
    let password = '';

    function updateAction() {
        start.textContent = skipValidation.checked ? '直接生成密码文件' : '验证并保存密码';
    }
    skipValidation.addEventListener('change', updateAction);
    updateAction();

    async function post(url, body) {
        const response = await fetch(url, {method: 'POST', cache: 'no-store',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({...body, csrf_token: form.elements.csrf_token.value})});
        let data;
        try { data = await response.json(); } catch { throw new Error('服务器响应异常，请刷新后重试'); }
        if (!response.ok) throw new Error(data.error || '请求失败，请稍后重试');
        return data;
    }
    stop.addEventListener('click', () => {
        stopping = true;
        stop.disabled = true;
        status.textContent = '正在停止：等待当前文件处理结束，不再开始后续文件。';
    });
    window.addEventListener('pagehide', () => { stopping = true; password = ''; input.value = ''; });
    form.addEventListener('submit', async event => {
        event.preventDefault();
        if (running || !form.reportValidity()) return;
        running = true;
        stopping = false;
        password = input.value;
        const skip = skipValidation.checked;
        input.value = '';
        input.disabled = true;
        skipValidation.disabled = true;
        start.disabled = true;
        stop.hidden = false;
        stop.disabled = false;
        results.replaceChildren();
        progress.hidden = true;
        status.textContent = '正在扫描当前目录…';
        const counts = {saved: 0, skipped: 0, failed: 0};
        let finished = 0;
        try {
            const {items} = await post(form.dataset.planUrl, {});
            const rows = items.map(item => {
                const row = document.createElement('li');
                const name = document.createElement('span');
                name.textContent = item.name;
                const outcome = document.createElement('span');
                outcome.textContent = item.message || '等待处理';
                row.append(name, outcome);
                results.append(row);
                return {item, row, outcome};
            });
            progress.max = Math.max(items.length, 1);
            progress.value = 0;
            progress.hidden = items.length === 0;
            for (const {item, row, outcome} of rows) {
                if (stopping) break;
                let result = item;
                if (item.status === 'pending') {
                    outcome.textContent = skip ? '正在生成密码文件…' : '正在验证并保存…';
                    status.textContent = `正在处理 ${finished + 1} / ${items.length}：${item.name}`;
                    try { result = await post(item.url, {password, skip_validation: skip}); }
                    catch (error) { result = {status: 'failed', message: error.message}; }
                }
                const state = Object.hasOwn(counts, result.status) ? result.status : 'failed';
                row.dataset.state = state;
                outcome.textContent = result.message || '处理失败，请重试';
                counts[state]++;
                progress.value = ++finished;
            }
            for (const {row, outcome} of rows) {
                if (!row.dataset.state) outcome.textContent = '未处理';
            }
            status.textContent = items.length === 0 ? '当前目录没有 ZIP 文件。' :
                `${stopping ? '已停止' : '处理完成'}：成功 ${counts.saved}，跳过 ${counts.skipped}，失败 ${counts.failed}` +
                (finished < items.length ? `，未处理 ${items.length - finished}` : '') + '。';
        } catch (error) {
            status.textContent = error.message || '处理失败，请重试';
        } finally {
            password = '';
            input.value = '';
            input.disabled = false;
            skipValidation.disabled = false;
            start.disabled = false;
            stop.hidden = true;
            running = false;
        }
    });
})();
