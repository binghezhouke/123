(() => {
    const form = document.getElementById('shared-password-form');
    if (!form) return;
    const password = document.getElementById('shared-password');
    const overwrite = document.getElementById('shared-password-overwrite');
    const button = form.querySelector('button[type="submit"]');
    const status = document.getElementById('shared-password-status');
    const current = document.getElementById('shared-password-current');
    form.addEventListener('submit', async event => {
        event.preventDefault();
        if (!form.reportValidity()) return;
        button.disabled = true;
        status.textContent = '正在保存…';
        try {
            const response = await fetch(form.dataset.url, {
                method: 'POST', cache: 'no-store',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({password: password.value, overwrite: overwrite.checked,
                    csrf_token: form.elements.csrf_token.value})
            });
            const data = await response.json();
            if (!response.ok) throw new Error(data.error || data.message || '保存失败');
            status.textContent = data.message || '已保存';
            if (data.status === 'saved' && current) current.textContent = password.value;
            password.value = '';
        } catch (error) {
            status.textContent = error.message || '保存失败，请重试';
        } finally {
            button.disabled = false;
        }
    });
})();
