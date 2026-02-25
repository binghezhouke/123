// CloudArchive - macOS Finder Style JavaScript

// Wait for DOM to load
document.addEventListener('DOMContentLoaded', () => {
    // Initialize tooltips
    const tooltipTriggerList = document.querySelectorAll('[data-bs-toggle="tooltip"]');
    tooltipTriggerList.forEach(el => new bootstrap.Tooltip(el));

    // Bind download buttons
    bindDownloadButtons();

    // Initialize card animations
    initCardAnimations();

    // Search enhancement
    initSearchEnhancement();

    // Folder/file click to open
    initItemClick();

    // Keyboard shortcuts
    initKeyboardShortcuts();

    // Auto-hide alerts
    initAutoHideAlerts();

    console.log('CloudArchive - Finder style loaded');

    // Performance monitoring
    if (window.performance && window.performance.timing) {
        const loadTime = window.performance.timing.loadEventEnd - window.performance.timing.navigationStart;
        console.log('Page load time:', loadTime + 'ms');
    }
});

/**
 * Bind download button events
 */
function bindDownloadButtons() {
    const downloadButtons = document.querySelectorAll('.download-btn');
    downloadButtons.forEach(button => {
        button.addEventListener('click', handleDownloadClick);
    });
}

/**
 * Handle download button click
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
 * Unified download file function
 * @param {string|number} fileId - File ID
 * @param {string} fileName - File name
 */
function downloadFile(fileId, fileName = '文件') {
    // Show modal
    const modal = new bootstrap.Modal(document.getElementById('downloadModal'));
    modal.show();

    // Reset modal content
    const modalBody = document.getElementById('downloadModalBody');
    const downloadLink = document.getElementById('downloadLink');
    const copyBtn = document.getElementById('copyDownloadLinkBtn');

    modalBody.innerHTML = `
        <p>正在获取 "${escapeHtml(fileName)}" 的下载链接...</p>
        <div class="text-center" style="padding: 20px;">
            <div class="spinner-border" role="status" style="color: var(--accent-blue);">
                <span class="visually-hidden">加载中...</span>
            </div>
        </div>
    `;
    downloadLink.style.display = 'none';
    copyBtn.style.display = 'none';

    // Request download link
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
                    <div class="alert alert-success" style="margin-bottom: 16px;">
                        <i class="fas fa-check-circle"></i> 下载链接获取成功！
                    </div>
                    <p style="font-size: 13px; color: var(--text-secondary); margin-bottom: 8px;">文件: <strong style="color: var(--text-primary);">${escapeHtml(fileName)}</strong></p>
                    <p style="font-size: 12px; color: var(--text-secondary);">点击下方按钮开始下载，链接可能有时效性。</p>
                `;
                downloadLink.href = data.download_url;
                downloadLink.style.display = 'inline-flex';
                copyBtn.style.display = 'inline-flex';
                copyBtn.dataset.url = data.download_url;
            } else {
                modalBody.innerHTML = `
                    <div class="alert alert-danger" style="margin-bottom: 16px;">
                        <i class="fas fa-exclamation-triangle"></i> 获取下载链接失败
                    </div>
                    <p style="font-size: 13px; color: var(--text-secondary);">无法获取文件 "${escapeHtml(fileName)}" 的下载链接</p>
                    <p style="font-size: 12px; color: var(--text-secondary);">${data.error || '未知错误'}</p>
                `;
                showToast('获取下载链接失败', 'error');
            }
        })
        .catch(error => {
            console.error('下载请求失败:', error);
            modalBody.innerHTML = `
                <div class="alert alert-danger" style="margin-bottom: 16px;">
                    <i class="fas fa-exclamation-triangle"></i> 下载失败
                </div>
                <p style="font-size: 13px; color: var(--text-secondary);">错误信息: ${escapeHtml(error.message)}</p>
                <p style="font-size: 12px; color: var(--text-secondary);">请检查网络连接或稍后重试</p>
            `;
            showToast('获取下载链接失败: ' + error.message, 'error');
        });
}

/**
 * Copy download URL
 */
function copyDownloadUrl() {
    const copyBtn = document.getElementById('copyDownloadLinkBtn');
    const url = copyBtn.dataset.url;

    if (url) {
        copyToClipboard(url);
    }
}

/**
 * Get download link (for inline calls in files.html)
 * @param {string|number} fileId - File ID
 */
function getDownloadLink(fileId) {
    downloadFile(fileId, '文件');
}

/**
 * Copy download link (for inline calls in files.html)
 */
function copyDownloadLink() {
    copyDownloadUrl();
}

/**
 * Initialize card animations
 */
function initCardAnimations() {
    const fileCards = document.querySelectorAll('.file-card, .file-grid-item, .file-list-row');
    fileCards.forEach((card, index) => {
        card.style.animationDelay = `${index * 0.03}s`;
        card.classList.add('fade-in');
    });
}

/**
 * Search enhancement
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

    // Toolbar search focus
    const toolbarSearch = document.getElementById('toolbarSearch');
    if (toolbarSearch) {
        toolbarSearch.addEventListener('focus', () => {
            toolbarSearch.parentElement.classList.add('focus');
        });
        toolbarSearch.addEventListener('blur', () => {
            toolbarSearch.parentElement.classList.remove('focus');
        });
    }
}

/**
 * Item single click to open
 */
function initItemClick() {
    const fileItems = document.querySelectorAll('.file-item');
    fileItems.forEach(item => {
        item.addEventListener('click', (e) => {
            // Don't navigate if clicking buttons or links directly
            if (e.target.closest('.btn') || e.target.tagName === 'A') return;

            const link = item.querySelector('a');
            if (link) {
                window.location.href = link.href;
            }
        });
    });
}

/**
 * Keyboard shortcuts
 */
function initKeyboardShortcuts() {
    document.addEventListener('keydown', (e) => {
        // Cmd/Ctrl + F focus search
        if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
            e.preventDefault();
            const searchInput = document.getElementById('toolbarSearch');
            if (searchInput) {
                searchInput.focus();
                searchInput.select();
            }
        }

        // ESC close modals
        if (e.key === 'Escape') {
            const openModals = document.querySelectorAll('.modal.show');
            openModals.forEach(modalEl => {
                const modal = bootstrap.Modal.getInstance(modalEl);
                if (modal) {
                    modal.hide();
                }
            });
        }

        // Enter to search when in search box
        if (e.key === 'Enter' && document.activeElement?.id === 'toolbarSearch') {
            const query = document.activeElement.value;
            if (query) {
                window.location.href = `/search?q=${encodeURIComponent(query)}`;
            }
        }
    });
}

/**
 * Auto-hide alerts
 */
function initAutoHideAlerts() {
    setTimeout(() => {
        const alerts = document.querySelectorAll('.alert:not(.alert-permanent):not(.alert-success):not(.alert-danger)');
        alerts.forEach(alert => {
            alert.style.transition = 'opacity 0.4s ease';
            alert.style.opacity = '0';
            setTimeout(() => alert.remove(), 400);
        });
    }, 5000);
}

// File operations
window.FileOperations = {
    /**
     * Get file info
     * @param {string|number} fileId - File ID
     * @param {function} callback - Callback function
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
     * Batch get file info
     * @param {Array} fileIds - File ID array
     * @param {function} callback - Callback function
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

// Utility functions

/**
 * Format file size
 * @param {number} bytes - Bytes
 * @returns {string} - Formatted string
 */
function formatFileSize(bytes) {
    if (bytes === 0) return '0 字节';

    const k = 1024;
    const sizes = ['字节', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));

    return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

/**
 * Format time
 * @param {number} timestamp - Unix timestamp
 * @returns {string} - Formatted string
 */
function formatTime(timestamp) {
    const date = new Date(timestamp * 1000);
    return date.toLocaleString('zh-CN');
}

/**
 * Escape HTML
 * @param {string} text - Raw text
 * @returns {string} - Escaped text
 */
function escapeHtml(text) {
    if (!text) return '';
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

/**
 * Copy to clipboard
 * @param {string} text - Text to copy
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
 * Fallback copy to clipboard
 * @param {string} text - Text to copy
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
 * Show Toast notification
 * @param {string} message - Message content
 * @param {string} type - Type: 'info', 'success', 'error', 'warning'
 */
function showToast(message, type = 'info') {
    const container = document.getElementById('toastContainer');
    if (!container) {
        console.error('Toast container not found');
        return;
    }

    // macOS style toast colors
    const colors = {
        success: { bg: 'rgba(52, 199, 89, 0.95)', icon: 'fa-check-circle' },
        error: { bg: 'rgba(255, 59, 48, 0.95)', icon: 'fa-exclamation-circle' },
        warning: { bg: 'rgba(255, 149, 0, 0.95)', icon: 'fa-exclamation-triangle' },
        info: { bg: 'rgba(0, 122, 255, 0.95)', icon: 'fa-info-circle' }
    };

    const { bg, icon } = colors[type] || colors.info;

    const toast = document.createElement('div');
    toast.className = 'toast align-items-center text-white border-0';
    toast.setAttribute('role', 'alert');
    toast.style.cssText = `
        background: ${bg};
        border-radius: 10px;
        box-shadow: 0 4px 12px rgba(0,0,0,0.15);
        backdrop-filter: blur(10px);
        -webkit-backdrop-filter: blur(10px);
        margin-bottom: 8px;
    `;
    toast.innerHTML = `
        <div class="d-flex">
            <div class="toast-body" style="padding: 12px 16px; font-size: 13px;">
                <i class="fas ${icon}" style="margin-right: 8px;"></i>${escapeHtml(message)}
            </div>
            <button type="button" class="btn-close btn-close-white me-2 m-auto" data-bs-dismiss="toast" aria-label="关闭" style="opacity: 0.8;"></button>
        </div>
    `;

    container.appendChild(toast);

    const toastInstance = new bootstrap.Toast(toast, { delay: 3000 });
    toastInstance.show();

    toast.addEventListener('hidden.bs.toast', () => {
        toast.remove();
    });
}

// Expose global functions
window.downloadFile = downloadFile;
window.getDownloadLink = getDownloadLink;
window.copyDownloadLink = copyDownloadLink;
window.copyDownloadUrl = copyDownloadUrl;
window.copyToClipboard = copyToClipboard;
window.showToast = showToast;
window.formatFileSize = formatFileSize;
window.formatTime = formatTime;
window.escapeHtml = escapeHtml;

// Refresh files
window.refreshFiles = function() {
    window.location.reload();
};

// Toggle view (placeholder for compatibility)
window.toggleView = function() {
    const container = document.getElementById('fileContainer');
    if (!container) return;

    // Call the new switchView function if available
    if (typeof switchView === 'function') {
        const iconBtn = document.getElementById('iconViewBtn');
        if (iconBtn && iconBtn.classList.contains('active')) {
            switchView('list');
        } else {
            switchView('icons');
        }
    }
};
