// 123云盘文件浏览器 JavaScript - ES6+ 版本

// 等待 DOM 加载完成
document.addEventListener('DOMContentLoaded', () => {
    // 初始化工具提示
    const tooltipTriggerList = document.querySelectorAll('[data-bs-toggle="tooltip"]');
    tooltipTriggerList.forEach(el => new bootstrap.Tooltip(el));

    // 绑定下载按钮事件
    bindDownloadButtons();

    // 文件卡片动画效果
    initCardAnimations();

    // 搜索表单增强
    initSearchEnhancement();

    // 文件夹双击进入
    initFolderDoubleClick();

    // 键盘快捷键
    initKeyboardShortcuts();

    // 自动隐藏提示消息
    initAutoHideAlerts();

    console.log('123云盘文件浏览器已加载完成');

    // 性能监控
    if (window.performance && window.performance.timing) {
        const loadTime = window.performance.timing.loadEventEnd - window.performance.timing.navigationStart;
        console.log('页面加载时间:', loadTime + 'ms');
    }
});

/**
 * 绑定下载按钮事件
 */
function bindDownloadButtons() {
    const downloadButtons = document.querySelectorAll('.download-btn');
    downloadButtons.forEach(button => {
        button.addEventListener('click', handleDownloadClick);
    });
}

/**
 * 处理下载按钮点击
 */
function handleDownloadClick(e) {
    const button = e.currentTarget;
    const fileId = button.dataset.fileId;
    const fileName = button.dataset.fileName || '文件';

    if (!fileId) {
        showToast('文件ID不存在', 'error');
        return;
    }

    downloadFile(fileId, fileName);
}

/**
 * 统一下载文件函数
 * @param {string|number} fileId - 文件ID
 * @param {string} fileName - 文件名
 */
function downloadFile(fileId, fileName = '文件') {
    // 显示模态框
    const modal = new bootstrap.Modal(document.getElementById('downloadModal'));
    modal.show();

    // 重置模态框内容
    const modalBody = document.getElementById('downloadModalBody');
    const downloadLink = document.getElementById('downloadLink');
    const copyBtn = document.getElementById('copyDownloadLinkBtn');

    modalBody.innerHTML = `
        <p>正在获取 "${escapeHtml(fileName)}" 的下载链接...</p>
        <div class="text-center">
            <div class="spinner-border text-amber" role="status">
                <span class="visually-hidden">加载中...</span>
            </div>
        </div>
    `;
    downloadLink.style.display = 'none';
    copyBtn.style.display = 'none';

    // 请求下载链接
    fetch(`/api/download/${fileId}`)
        .then(response => {
            if (!response.ok) {
                throw new Error(`HTTP ${response.status}: ${response.statusText}`);
            }
            return response.json();
        })
        .then(data => {
            if (data.success && data.download_url) {
                modalBody.innerHTML = `
                    <div class="alert alert-success">
                        <i class="fas fa-check-circle"></i> 下载链接获取成功！
                    </div>
                    <p>文件: <strong>${escapeHtml(fileName)}</strong></p>
                    <p class="text-muted small">点击下方按钮开始下载，链接可能有时效性。</p>
                `;
                downloadLink.href = data.download_url;
                downloadLink.style.display = 'inline-block';
                copyBtn.style.display = 'inline-block';
                copyBtn.dataset.url = data.download_url;
            } else {
                modalBody.innerHTML = `
                    <div class="alert alert-danger">
                        <i class="fas fa-exclamation-triangle"></i> 获取下载链接失败
                    </div>
                    <p>无法获取文件 "${escapeHtml(fileName)}" 的下载链接</p>
                    <p class="text-muted small">${data.error || '未知错误'}</p>
                `;
                showToast('获取下载链接失败', 'error');
            }
        })
        .catch(error => {
            console.error('下载请求失败:', error);
            modalBody.innerHTML = `
                <div class="alert alert-danger">
                    <i class="fas fa-exclamation-triangle"></i> 下载失败
                </div>
                <p>错误信息: ${escapeHtml(error.message)}</p>
                <p class="text-muted small">请检查网络连接或稍后重试</p>
            `;
            showToast('获取下载链接失败: ' + error.message, 'error');
        });
}

/**
 * 复制下载链接
 */
function copyDownloadUrl() {
    const copyBtn = document.getElementById('copyDownloadLinkBtn');
    const url = copyBtn.dataset.url;

    if (url) {
        copyToClipboard(url);
    }
}

/**
 * 获取下载链接 (用于 files.html 中的内联调用)
 * @param {string|number} fileId - 文件ID
 */
function getDownloadLink(fileId) {
    downloadFile(fileId, '文件');
}

/**
 * 复制下载链接 (用于 files.html 中的内联调用)
 */
function copyDownloadLink() {
    copyDownloadUrl();
}

/**
 * 初始化文件卡片动画
 */
function initCardAnimations() {
    const fileCards = document.querySelectorAll('.file-card');
    fileCards.forEach((card, index) => {
        card.style.animationDelay = `${index * 0.1}s`;
        card.classList.add('fade-in');
    });
}

/**
 * 搜索表单增强
 */
function initSearchEnhancement() {
    const searchQuery = document.getElementById('searchQuery');
    if (searchQuery) {
        searchQuery.addEventListener('input', (e) => {
            const query = e.target.value.trim();
            const submitBtn = e.target.closest('form')?.querySelector('button[type="submit"]');
            if (submitBtn) {
                submitBtn.disabled = query.length === 0;
            }
        });
    }
}

/**
 * 文件夹双击进入
 */
function initFolderDoubleClick() {
    const fileCards = document.querySelectorAll('.file-card');
    fileCards.forEach(card => {
        card.addEventListener('dblclick', () => {
            const link = card.querySelector('a');
            if (link) {
                window.location.href = link.href;
            }
        });
    });
}

/**
 * 键盘快捷键
 */
function initKeyboardShortcuts() {
    document.addEventListener('keydown', (e) => {
        // Ctrl/Cmd + F 聚焦搜索框
        if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
            e.preventDefault();
            const searchInput = document.querySelector('.navbar .search-input[name="q"]');
            if (searchInput) {
                searchInput.focus();
                searchInput.select();
            }
        }

        // ESC 关闭模态框
        if (e.key === 'Escape') {
            const openModals = document.querySelectorAll('.modal.show');
            openModals.forEach(modalEl => {
                const modal = bootstrap.Modal.getInstance(modalEl);
                if (modal) {
                    modal.hide();
                }
            });
        }

        // 回车键搜索
        if (e.key === 'Enter' && document.activeElement?.name === 'q') {
            document.activeElement.closest('form')?.submit();
        }
    });
}

/**
 * 自动隐藏提示消息
 */
function initAutoHideAlerts() {
    setTimeout(() => {
        const alerts = document.querySelectorAll('.alert:not(.alert-permanent)');
        alerts.forEach(alert => {
            alert.style.transition = 'opacity 0.5s ease';
            alert.style.opacity = '0';
            setTimeout(() => alert.remove(), 500);
        });
    }, 5000);
}

// 文件操作相关函数
window.FileOperations = {
    /**
     * 获取文件详情
     * @param {string|number} fileId - 文件ID
     * @param {function} callback - 回调函数
     */
    getFileInfo: function(fileId, callback) {
        fetch(`/api/files/batch?ids=${fileId}`)
            .then(response => response.json())
            .then(data => {
                if (data.success && data.files && data.files.length > 0) {
                    callback(null, data.files[0]);
                } else {
                    callback('文件不存在');
                }
            })
            .catch(error => {
                callback(error.message || '获取文件信息失败');
            });
    },

    /**
     * 批量获取文件详情
     * @param {Array} fileIds - 文件ID数组
     * @param {function} callback - 回调函数
     */
    getBatchFileInfo: function(fileIds, callback) {
        const ids = fileIds.join(',');
        fetch(`/api/files/batch?ids=${ids}`)
            .then(response => response.json())
            .then(data => {
                if (data.success) {
                    callback(null, data.files);
                } else {
                    callback(data.error || '获取文件信息失败');
                }
            })
            .catch(error => {
                callback(error.message || '获取文件信息失败');
            });
    }
};

// 工具函数

/**
 * 文件大小格式化
 * @param {number} bytes - 字节数
 * @returns {string} - 格式化后的字符串
 */
function formatFileSize(bytes) {
    if (bytes === 0) return '0 字节';

    const k = 1024;
    const sizes = ['字节', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));

    return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

/**
 * 时间格式化
 * @param {number} timestamp - Unix时间戳
 * @returns {string} - 格式化后的字符串
 */
function formatTime(timestamp) {
    const date = new Date(timestamp * 1000);
    return date.toLocaleString('zh-CN');
}

/**
 * HTML转义
 * @param {string} text - 原始文本
 * @returns {string} - 转义后的文本
 */
function escapeHtml(text) {
    if (!text) return '';
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

/**
 * 复制到剪贴板
 * @param {string} text - 要复制的文本
 */
function copyToClipboard(text) {
    if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text)
            .then(() => showToast('已复制到剪贴板', 'success'))
            .catch(() => fallbackCopyToClipboard(text));
    } else {
        fallbackCopyToClipboard(text);
    }
}

/**
 * 复制到剪贴板（降级方案）
 * @param {string} text - 要复制的文本
 */
function fallbackCopyToClipboard(text) {
    const textArea = document.createElement('textarea');
    textArea.value = text;
    textArea.style.top = '0';
    textArea.style.left = '0';
    textArea.style.position = 'fixed';
    textArea.style.opacity = '0';

    document.body.appendChild(textArea);
    textArea.focus();
    textArea.select();

    try {
        const successful = document.execCommand('copy');
        if (successful) {
            showToast('已复制到剪贴板', 'success');
        } else {
            showToast('复制失败', 'error');
        }
    } catch (err) {
        showToast('复制失败', 'error');
    }

    document.body.removeChild(textArea);
}

/**
 * 显示 Toast 通知
 * @param {string} message - 消息内容
 * @param {string} type - 类型: 'info', 'success', 'error', 'warning'
 */
function showToast(message, type = 'info') {
    const container = document.getElementById('toastContainer');
    if (!container) {
        console.error('Toast container not found');
        return;
    }

    const bgClass = type === 'error' ? 'bg-danger' :
                   type === 'success' ? 'bg-success' :
                   type === 'warning' ? 'bg-warning text-dark' :
                   'bg-primary';

    const icon = type === 'error' ? 'fa-exclamation-circle' :
                type === 'success' ? 'fa-check-circle' :
                type === 'warning' ? 'fa-exclamation-triangle' :
                'fa-info-circle';

    const toast = document.createElement('div');
    toast.className = `toast align-items-center text-white ${bgClass} border-0`;
    toast.setAttribute('role', 'alert');
    toast.innerHTML = `
        <div class="d-flex">
            <div class="toast-body">
                <i class="fas ${icon} me-2"></i>${escapeHtml(message)}
            </div>
            <button type="button" class="btn-close btn-close-white me-2 m-auto" data-bs-dismiss="toast" aria-label="关闭"></button>
        </div>
    `;

    container.appendChild(toast);

    const toastInstance = new bootstrap.Toast(toast, { delay: 3000 });
    toastInstance.show();

    toast.addEventListener('hidden.bs.toast', () => {
        toast.remove();
    });
}

// 暴露全局函数供内联脚本调用
window.downloadFile = downloadFile;
window.getDownloadLink = getDownloadLink;
window.copyDownloadLink = copyDownloadLink;
window.copyDownloadUrl = copyDownloadUrl;
window.copyToClipboard = copyToClipboard;
window.showToast = showToast;
window.formatFileSize = formatFileSize;
window.formatTime = formatTime;
window.escapeHtml = escapeHtml;

// 刷新文件列表（用于 files.html）
window.refreshFiles = function() {
    window.location.reload();
};

// 切换视图（用于 files.html）
window.toggleView = function() {
    const container = document.getElementById('fileContainer');
    if (!container) return;

    container.classList.toggle('row');
    const items = container.querySelectorAll('.file-item');
    items.forEach(item => {
        item.classList.toggle('col-12');
        item.classList.toggle('col-md-6');
        item.classList.toggle('col-lg-4');
    });
};
