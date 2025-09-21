# 123 云盘文件浏览器 AI 指导

欢迎！本文档旨在帮助你快速理解此代码库并高效地做出贡献。

## 1. 项目概述

这是一个基于 Flask 的 Web 应用，作为第三方“123 云盘”的图形化文件浏览器。它通过调用官方的 `open-api.123pan.com` API 来实现文件列表、搜索、详情查看和下载链接获取等功能。

项目核心分为两大部分：

1.  **Flask 前端** (`app.py`, `templates/`, `static/`): 处理用户请求，渲染页面。
2.  **123 云盘 API 客户端库** (`api/`): 封装了与云盘 API 交互的所有逻辑，是一个独立的、可重用的组件。

## 2. 关键架构和数据流

理解 `api/` 目录的结构至关重要。所有与外部 API 的交互都由这个库处理。

- **主入口点**: `api.client.Pan123Client` 是与 API 交互的唯一公共接口。Flask 应用 (`app.py`) 只与这个类通信。
- **关注点分离**: `Pan123Client` 将具体任务委托给专门的模块：
  - `api.file_service.FileService`: 实现所有文件相关的业务逻辑（如列出文件、获取信息、生成 WebDAV URL）。
  - `api.http_client.RequestHandler`: 负责构造和发送所有 HTTP 请求，并处理标准的 API 响应和错误。
  - `api.auth.TokenManager`: 自动管理 API 的 `access_token`，包括获取、缓存和刷新。
  - `api.cache.FileCacheManager`: 使用 Redis 缓存文件元数据（如文件详情、路径），以减少不必要的 API 调用。
  - `api.config.ConfigManager`: 从 `config.json` 文件中安全地读取配置信息（如客户端凭据、WebDAV 设置）。
  - `api.models.py`: 定义了核心数据结构，如 `File` 和 `File`，用于封装 API 返回的数据，提供了类型安全和便捷的方法。

**典型数据流 (例如：列出文件):**

1.  浏览器请求 `app.py` 中的 `/` 路由。
2.  路由调用 `client.list_files()` (`client` 是 `Pan123Client` 的实例)。
3.  `Pan123Client` 将调用委托给 `file_service.list_files()`。
4.  `FileService` 调用 `http_client.get()` 来请求云盘 API。
5.  `RequestHandler` 在发送请求前，会向 `token_manager.ensure_valid_token()` 请求一个有效的令牌。
6.  `FileService` 收到响应后，可能会将结果存入 `cache_manager`。
7.  数据被包装成 `FileList` 对象并返回给 `app.py`。
8.  `app.py` 将 `FileList` 对象传递给 Jinja2 模板进行渲染。

## 3. 开发工作流

### 设置

1.  **安装依赖**: 项目使用 `uv` 进行包管理。运行 `uv pip install -r requirements.txt` 来安装依赖。
2.  **配置**:
    - 复制 `config.json.template` 到 `config.json`。
    - 在 `config.json` 中填入你的 `clientID` 和 `clientSecret`。
    - 确保 Redis 服务正在运行，并根据需要更新 `app.py` 中 `init_client` 函数的 Redis 连接参数。

### 运行应用

直接运行主应用文件即可启动开发服务器：

```bash
python app.py
```

服务器将运行在 `http://localhost:8080`。

## 4. 项目约定

- **模型优先**: API 的原始 JSON 响应会被立即转换成 `api/models.py` 中定义的 `File` 或 `FileList` 对象。在整个应用中（包括模板）都应传递和使用这些对象，而不是原始字典，以确保代码的清晰和可维护性。
- **缓存策略**: `FileService` 和 `FileCacheManager` 负责缓存逻辑。默认情况下，获取文件详情会优先从 Redis 缓存中读取。在修改了可能影响缓存数据的逻辑时，请考虑缓存的更新或失效策略。
- **错误处理**: API 相关的错误应在 `api/` 库中被捕获，并重新抛出为自定义的 `Pan123APIError` 异常。Flask 应用层 (`app.py`) 负责捕获此异常并向用户显示友好的错误消息。
- **配置管理**: 严禁在代码中硬编码任何敏感信息（如密钥、密码）。所有配置都应通过 `ConfigManager` 从 `config.json` 加载。

---

这份文档是否清晰？有没有哪些部分你觉得不够详细或者有疑问，我可以为你进一步说明。
