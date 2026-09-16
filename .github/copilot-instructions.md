# 123 云盘工具箱 AI 指导

本文档帮助你快速理解此代码库并高效地做出贡献。面向用户的使用说明见根目录 `README.md`。

## 1. 项目概述

三部分：

1. **客户端库** (`api/`)：封装 `open-api.123pan.com`，是独立可复用的组件。唯一公共入口是 `api.client.Pan123Client`。
2. **Flask 前端** (`app.py`, `routes/`, `templates/`, `static/`)：只与 `Pan123Client` 通信。
3. **上传脚本** (`upload_core.py` + `upload_folder.py` / `upload_from_json.py` / `batch_upload.py`)：命令行批量上传。

## 2. 关键架构和数据流

- `Pan123Client` 把任务委托给专门模块：
  - `api/file_service.FileService`：文件列表/搜索、路径、下载地址、WebDAV、分钟目录创建、上传与秒传。
  - `api/http_client.RequestHandler`：构造和发送 HTTP 请求，统一处理重试（5xx、业务码 429 / 20103）。
  - `api/auth.TokenManager`：token 获取/缓存/刷新。缓存固定在 `~/.cache/pan123_api/token.json`（0600 权限）。
  - `api/cache.FileCacheManager`：Redis 缓存文件元数据，JSON 序列化（不要改回 pickle）。
  - `api/config.ConfigManager`：读取 `config.json`（WebDAV 键名 `USERNAME` / `BASE_URL` / `PATH_PREFIX`，兼容旧名 `USER` / `HOST`）。
  - `api/models.py`：`File` / `FileList` 数据模型，模板里用的就是这些对象。
- **典型数据流（列出文件）**：路由 `routes/main.py` → `Pan123Client.list_files()` → `FileService.list_files()` → `RequestHandler.get()`（发请求前经 `TokenManager` 取有效 token）→ 结果包装成 `FileList` 交给 Jinja2 模板。
- **上传链路**：`upload_core.py` 负责清单解析、哈希归一化、`RemoteDirTree` 目录缓存、`RemoteIndex` 目录快照判重、`ExportTreeIndex` 目录树导出索引（`--tree`）、限速、秒传执行和 `UploadStats` 统计；三个 CLI 只做参数解析和输出。判重只看云端现状，不依赖任何本地"已完成"记录。改动上传行为请改 `upload_core.py`，不要在脚本里各写一份。

## 3. 开发工作流

```bash
uv sync --group dev      # 安装依赖（含 pytest / ruff）
uv run python app.py     # 启动 Web 应用，http://localhost:8080
uv run pytest -q         # 跑测试
uv run ruff check .      # 静态检查
```

CI 见 `.github/workflows/ci.yml`，push / PR 时执行上面两条命令。

配置：`cp config.json.template config.json`，填入 `CLIENT_ID` / `CLIENT_SECRET`。Redis 可选，连不上会自动降级。测试不需要网络、Redis 或真实配置。

## 4. 项目约定

- **模型优先**：API 原始 JSON 立即转成 `File` / `FileList` 对象再往下传，模板里也用这些对象。
- **秒传口径**：秒传接口只有 `reuse=true` 算成功；返回 `None` 表示"云端没有该文件"；调用失败必须抛异常。统计时不能把失败算作"未命中"。见 `upload_core.STATUS_*`。
- **分页**：结束判断只能看服务端返回的原始条数或 `lastFileId == -1`。`_fetch_page` 会过滤回收站记录，用过滤后的条数判断会提前截断结果。
- **错误处理**：API 错误在 `api/` 内统一抛 `Pan123APIError` 及子类，Flask 层负责转成友好提示。
- **重试**：只在 `api/http_client.py` 里做，业务层不要再加一套。
- **敏感信息**：不要硬编码任何密钥、密码、token；不要把这些内容写进 md 文档（历史上有过一次明文密码进仓库的事故）。WebDAV 密码会拼进 URL，日志输出必须走 `_mask_credentials`。
- **限流**：同一 `client_id` 最多 3 个 token（`TokenManager` 里有锁 + 内存缓存，避免并发刷新把 token 挤掉）；限流是账号级的，超过约 8 请求/秒会返回业务码 1「请慢一点」，`RequestHandler` 已把它加入重试白名单并带抖动退避，批量上传通过 `api/ratelimit.TokenBucket` 主动限速；目录创建保持串行。
- **判重**：目标目录快照（`RemoteIndex`）里文件记录带 32 位 MD5 etag、目录记录带目录ID，所以判重和"免建目录"都靠列目录实现。清单只有 SHA1 时按同名同大小弱判等，MD5 冲突要单独报出来而不是静默跳过。
