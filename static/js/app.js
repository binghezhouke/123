// 123云盘文件浏览器 JavaScript 功能

$(document).ready(function() {
    // 初始化工具提示
    var tooltipTriggerList = [].slice.call(document.querySelectorAll('[data-bs-toggle="tooltip"]'));
    var tooltipList = tooltipTriggerList.map(function (tooltipTriggerEl) {
        return new bootstrap.Tooltip(tooltipTriggerEl);
    });

    // 下载按钮点击事件
    $('.download-btn').on('click', function() {
        var fileId = $(this).data('file-id');
        var fileName = $(this).data('file-name');
        
        if (!fileId) {
            alert('文件ID不存在');
            return;
        }
        
        // 显示模态框
        var modal = new bootstrap.Modal(document.getElementById('downloadModal'));
        modal.show();
        
        // 重置模态框内容
        $('#downloadModal .modal-body').html(`
            <p>正在获取 "${fileName}" 的下载链接...</p>
            <div class="text-center">
                <div class="spinner-border" role="status">
                    <span class="visually-hidden">加载中...</span>
                </div>
            </div>
        `);
        $('#downloadLink').hide();
        
        // 请求下载链接
        $.ajax({
            url: '/api/download/' + fileId,
            method: 'GET',
            timeout: 30000,
            success: function(response) {
                if (response.success && response.download_url) {
                    $('#downloadModal .modal-body').html(`
                        <div class="alert alert-success">
                            <i class="fas fa-check-circle"></i> 下载链接获取成功！
                        </div>
                        <p>文件: <strong>${fileName}</strong></p>
                        <p class="text-muted small">点击下方按钮开始下载，链接可能有时效性。</p>
                    `);
                    $('#downloadLink').attr('href', response.download_url).show();
                } else {
                    $('#downloadModal .modal-body').html(`
                        <div class="alert alert-danger">
                            <i class="fas fa-exclamation-triangle"></i> 获取下载链接失败
                        </div>
                        <p>无法获取文件 "${fileName}" 的下载链接</p>
                    `);
                }
            },
            error: function(xhr, status, error) {
                var errorMsg = '未知错误';
                
                if (xhr.responseJSON && xhr.responseJSON.error) {
                    errorMsg = xhr.responseJSON.error;
                } else if (status === 'timeout') {
                    errorMsg = '请求超时，请稍后重试';
                } else if (status === 'abort') {
                    errorMsg = '请求被取消';
                } else if (xhr.status === 0) {
                    errorMsg = '网络连接失败';
                } else {
                    errorMsg = `HTTP ${xhr.status}: ${error}`;
                }
                
                $('#downloadModal .modal-body').html(`
                    <div class="alert alert-danger">
                        <i class="fas fa-exclamation-triangle"></i> 下载失败
                    </div>
                    <p>错误信息: ${errorMsg}</p>
                    <p class="text-muted small">请检查网络连接或稍后重试</p>
                `);
            }
        });
    });
    
    // 文件卡片动画效果
    $('.file-card').each(function(index) {
        $(this).css('animation-delay', (index * 0.1) + 's');
        $(this).addClass('fade-in');
    });
    
    // 搜索表单增强
    $('#searchQuery').on('input', function() {
        var query = $(this).val().trim();
        if (query.length === 0) {
            $('#searchForm .btn[type="submit"]').prop('disabled', true);
        } else {
            $('#searchForm .btn[type="submit"]').prop('disabled', false);
        }
    });
    
    // 文件夹双击进入
    $('.file-card').on('dblclick', function() {
        var link = $(this).find('a').first();
        if (link.length > 0) {
            window.location.href = link.attr('href');
        }
    });
    
    // 键盘快捷键
    $(document).on('keydown', function(e) {
        // Ctrl/Cmd + F 聚焦搜索框
        if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
            e.preventDefault();
            var searchInput = $('.navbar .form-control[name="q"]');
            if (searchInput.length > 0) {
                searchInput.focus().select();
            }
        }
        
        // ESC 关闭模态框
        if (e.key === 'Escape') {
            $('.modal.show').each(function() {
                var modal = bootstrap.Modal.getInstance(this);
                if (modal) {
                    modal.hide();
                }
            });
        }
        
        // 回车键搜索
        if (e.key === 'Enter' && document.activeElement.name === 'q') {
            $(document.activeElement).closest('form').submit();
        }
    });
    
    // 自动隐藏提示消息
    setTimeout(function() {
        $('.alert:not(.alert-permanent)').fadeOut(500);
    }, 5000);
    
    // 批量操作功能 (未来扩展)
    var selectedFiles = [];
    
    $('.file-checkbox').on('change', function() {
        var fileId = $(this).data('file-id');
        if ($(this).is(':checked')) {
            if (selectedFiles.indexOf(fileId) === -1) {
                selectedFiles.push(fileId);
            }
        } else {
            var index = selectedFiles.indexOf(fileId);
            if (index > -1) {
                selectedFiles.splice(index, 1);
            }
        }
        
        updateBatchActions();
    });
    
    function updateBatchActions() {
        var batchActionsPanel = $('#batchActions');
        if (selectedFiles.length > 0) {
            batchActionsPanel.show();
            batchActionsPanel.find('.selected-count').text(selectedFiles.length);
        } else {
            batchActionsPanel.hide();
        }
    }
    
    // 全选/取消全选
    $('#selectAll').on('change', function() {
        var isChecked = $(this).is(':checked');
        $('.file-checkbox').prop('checked', isChecked).trigger('change');
    });
    
    // 文件大小格式化工具函数
    window.formatFileSize = function(bytes) {
        if (bytes === 0) return '0 字节';
        
        var k = 1024;
        var sizes = ['字节', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(bytes) / Math.log(k));
        
        return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
    };
    
    // 时间格式化工具函数
    window.formatTime = function(timestamp) {
        var date = new Date(timestamp * 1000);
        return date.toLocaleString('zh-CN');
    };
    
    // 复制到剪贴板功能
    window.copyToClipboard = function(text) {
        if (navigator.clipboard && window.isSecureContext) {
            navigator.clipboard.writeText(text).then(function() {
                showToast('已复制到剪贴板', 'success');
            }).catch(function() {
                fallbackCopyToClipboard(text);
            });
        } else {
            fallbackCopyToClipboard(text);
        }
    };
    
    function fallbackCopyToClipboard(text) {
        var textArea = document.createElement("textarea");
        textArea.value = text;
        textArea.style.top = "0";
        textArea.style.left = "0";
        textArea.style.position = "fixed";
        
        document.body.appendChild(textArea);
        textArea.focus();
        textArea.select();
        
        try {
            document.execCommand('copy');
            showToast('已复制到剪贴板', 'success');
        } catch (err) {
            showToast('复制失败', 'error');
        }
        
        document.body.removeChild(textArea);
    }
    
    // 显示Toast消息
    window.showToast = function(message, type = 'info') {
        var toast = $(`
            <div class="toast align-items-center text-white bg-${type === 'error' ? 'danger' : (type === 'success' ? 'success' : 'primary')} border-0" role="alert">
                <div class="d-flex">
                    <div class="toast-body">
                        ${message}
                    </div>
                    <button type="button" class="btn-close btn-close-white me-2 m-auto" data-bs-dismiss="toast"></button>
                </div>
            </div>
        `);
        
        // 添加Toast容器（如果不存在）
        if ($('.toast-container').length === 0) {
            $('body').append('<div class="toast-container position-fixed bottom-0 end-0 p-3"></div>');
        }
        
        $('.toast-container').append(toast);
        
        var toastInstance = new bootstrap.Toast(toast[0]);
        toastInstance.show();
        
        // 自动移除
        toast.on('hidden.bs.toast', function () {
            $(this).remove();
        });
    };
    
    // 页面加载完成提示
    console.log('123云盘文件浏览器已加载完成');
    
    // 性能监控
    if (window.performance && window.performance.timing) {
        var loadTime = window.performance.timing.loadEventEnd - window.performance.timing.navigationStart;
        console.log('页面加载时间:', loadTime + 'ms');
    }
});

// 文件操作相关函数
window.FileOperations = {
    // 获取文件详情
    getFileInfo: function(fileId, callback) {
        $.ajax({
            url: '/api/files/batch',
            method: 'GET',
            data: { ids: fileId },
            success: function(response) {
                if (response.success && response.files && response.files.length > 0) {
                    callback(null, response.files[0]);
                } else {
                    callback('文件不存在');
                }
            },
            error: function(xhr) {
                var error = xhr.responseJSON ? xhr.responseJSON.error : '获取文件信息失败';
                callback(error);
            }
        });
    },
    
    // 批量获取文件详情
    getBatchFileInfo: function(fileIds, callback) {
        $.ajax({
            url: '/api/files/batch',
            method: 'GET',
            data: { ids: fileIds.join(',') },
            success: function(response) {
                if (response.success) {
                    callback(null, response.files);
                } else {
                    callback('获取文件信息失败');
                }
            },
            error: function(xhr) {
                var error = xhr.responseJSON ? xhr.responseJSON.error : '获取文件信息失败';
                callback(error);
            }
        });
    }
};
