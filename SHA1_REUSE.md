# SHA1 秒传功能说明

## 功能概述

SHA1 秒传是 123 云盘提供的一项高效上传功能。通过计算文件的 SHA1 哈希值，系统可以在上传前检测云盘是否已经存在相同内容的文件。如果存在，则直接返回文件 ID，无需重新上传文件内容，大大提升上传效率。

## API 详情

### 接口地址
```
POST https://open-api.123pan.com/upload/v2/file/sha1_reuse
```

### 请求参数

| 参数名 | 类型 | 必填 | 说明 |
|--------|------|------|------|
| parentFileID | number | 是 | 父目录ID，上传到根目录时填写 0 |
| filename | string | 是 | 文件名（小于255字符，不能包含 "\/:*?\|><） |
| sha1 | string | 是 | 文件的 SHA1 哈希值 |
| size | number | 是 | 文件大小（单位：字节） |
| duplicate | number | 否 | 文件名冲突处理策略：1=保留两者，2=覆盖原文件 |

### 响应参数

| 参数名 | 类型 | 说明 |
|--------|------|------|
| fileID | number | 文件ID（秒传成功时返回） |
| reuse | boolean | 是否秒传成功 |

## 使用方法

### 1. 基本用法

```python
from api.client import Pan123Client

# 初始化客户端
client = Pan123Client()

# 上传文件（默认启用SHA1秒传）
result = client.upload_file(
    local_path="myfile.txt",
    parent_id=0,
    filename="myfile.txt"
)

# 检查是否秒传
if result.get('method') == 'sha1_reuse':
    print("SHA1秒传成功！")
elif result.get('reuse'):
    print("预上传秒传成功！")
else:
    print("常规上传成功")
```

### 2. 禁用 SHA1 秒传

```python
# 如果需要禁用SHA1秒传
result = client.upload_file(
    local_path="myfile.txt",
    parent_id=0,
    try_sha1_reuse=False  # 禁用SHA1秒传
)
```

### 3. 文件夹上传

文件夹上传脚本已自动集成 SHA1 秒传功能：

```bash
python upload_folder.py /path/to/folder remote/path
```

上传日志会显示每个文件的上传方式：
- `(SHA1秒传)` - 通过 SHA1 秒传
- `(秒传)` - 通过预上传 MD5 秒传
- 无标记 - 常规分片上传

## 工作流程

```
1. 计算本地文件的 SHA1 值
   ↓
2. 调用 SHA1 秒传 API
   ↓
3. 如果云盘已有相同文件 → 返回文件ID（秒传成功）
   ↓
4. 如果云盘没有该文件 → 继续常规上传流程
   ↓
   4a. 计算 MD5
   ↓
   4b. 预上传检查（MD5秒传）
   ↓
   4c. 如未命中，分片上传
```

## 优势

1. **极速上传**：相同内容的文件无需重复上传
2. **节省带宽**：减少数据传输量
3. **节省时间**：大文件秒传效果尤其明显
4. **智能回退**：秒传失败自动使用常规上传

## 示例脚本

### demo_sha1.py

演示 SHA1 秒传功能的完整示例：

```bash
python demo_sha1.py
```

该脚本会：
1. 创建测试文件
2. 首次上传（可能是常规上传）
3. 上传相同内容的文件（触发SHA1秒传）
4. 对比启用/禁用秒传的区别

### test_sha1_reuse.py

完整的功能测试脚本：

```bash
python test_sha1_reuse.py
```

## 注意事项

1. **文件名限制**：文件名必须小于 255 个字符，不能包含特殊字符 `"\/:*?|><`
2. **哈希计算**：大文件计算 SHA1 需要时间，但通常比上传快得多
3. **网络要求**：秒传 API 调用仍需网络连接
4. **重名处理**：使用 `duplicate` 参数控制重名策略

## 技术实现

核心实现在 `api/file_service.py` 中：

```python
def try_sha1_reuse(self, local_path, filename, parent_id, duplicate=1):
    """尝试使用SHA1秒传"""
    # 1. 计算文件SHA1和大小
    file_size = os.path.getsize(local_path)
    sha1_hash = self._calculate_sha1(local_path)
    
    # 2. 调用秒传API
    result = self.http_client.post("/upload/v2/file/sha1_reuse", {
        "parentFileID": parent_id,
        "filename": filename,
        "sha1": sha1_hash,
        "size": file_size,
        "duplicate": duplicate
    })
    
    # 3. 检查是否秒传成功
    if result and result.get('data', {}).get('reuse'):
        return result['data']
    
    return None
```

## 相关文件

- `api/file_service.py` - SHA1 秒传核心实现
- `api/client.py` - 客户端接口封装
- `upload_folder.py` - 文件夹上传（已集成秒传）
- `demo_sha1.py` - 功能演示脚本
- `test_sha1_reuse.py` - 完整测试脚本

## 更新日志

### 2026-01-27
- ✅ 实现 SHA1 秒传 API 调用
- ✅ 集成到 `upload_file` 方法
- ✅ 更新文件夹上传脚本
- ✅ 添加演示和测试脚本
- ✅ 支持启用/禁用秒传选项
