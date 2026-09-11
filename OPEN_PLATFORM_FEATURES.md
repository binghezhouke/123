# 123 云盘开放平台：新增功能建议

调研日期：2026-09-11。对照版本：ad9a9ff 及当前工作区（含未跟踪的 batch_upload.py）。仅调研和代码阅读，没有调用云盘业务接口或实现新功能。

官方入口：https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced

## 结论

最适合当前项目的新增能力是：离线下载任务、批量文件整理、分享链接、小文件单步上传，以及容量和权益信息。直链管理适合作为后续扩展。先让批处理可靠报告成功和失败，再把异步任务接入，收益最大。

以下优先级是结合仓库用途的设计判断，不是官方承诺。接口事实来自官方公开正文：网页读取工具失败后，通过公开页面的 /markdown 视图读取嵌入正文。目录中的功能名称与已核实的请求参数区分处理。

## 当前覆盖

- api/client.py、api/file_service.py：文件列表/搜索、详情、路径、下载地址、WebDAV、创建目录、本地分片上传、SHA1/MD5 复用、缓存。
- upload_folder.py、upload_from_json.py：目录上传和清单导入；batch_upload.py 提供正在开发的批处理与完成日志。
- routes/main.py、routes/api.py：浏览、搜索、详情与下载；没有文件整理、离线下载或分享的操作入口。
- SHA1 复用已实现，不能算本项目的新功能。

## 推荐功能

### 1. 离线下载任务（优先，工作量中等）

让用户粘贴资源 URL，选择云盘目录，查看排队/下载/重试/失败/完成状态。适合批量迁入远程资源，减少本机中转。

官方支持 POST /api/v1/offline/download，返回 taskID；GET /api/v1/offline/download/process 查询任务进度。只支持 HTTP/HTTPS，不能据此承诺磁力或 BT；不支持根目录，未指定目录时使用默认离线下载目录。[创建任务](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/he47hsq2o1xvgado)、[任务进度](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/sclficr3t655pii5)。

实现建议：先保存 taskID 并轮询，进程重启后继续跟踪。查询状态 0/1/2/3 分别表示进行中/失败/成功/重试中；回调的状态编码不同，不可共用枚举。文档对可选 fileName 的格式说明较特殊，回调错误字段也存在 fileReason/failReason 拼写差异，第一版先不依赖这两个字段。

### 2. 批量文件整理与复制（优先，工作量中等）

在现有列表页加入多选、重命名、移动、复制和回收站恢复。复制适合云盘内部整理副本；不应宣传为独立备份。

- 批量重命名：POST /api/v1/file/rename，一次最多 30 个，分别返回成功和失败结果。[官方文档](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/imhguepnr727aquk)
- 移动：POST /api/v1/file/move，单级最多 100 个。[官方文档](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/rsyfsn1gnpgo4m4f)
- 单文件复制：POST /api/v1/file/copy；批量复制：POST /api/v1/file/async/copy，单级最多 3000 个，返回 taskId，另有进度查询。不要把提交成功当成复制完成。[单文件](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/thpz0w9er500pob9)、[批量复制](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/pik0i4lvxw4lkkc7)、[进度](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/fqh9vk1esg4uomly)
- 恢复到指定目录：POST /api/v1/file/recover/by_path，一次最多 100 个。[官方文档](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/cl24atug2sviq12z)

实现前提：列表现在无条件过滤 trashed，需要增加明确的回收站浏览方式；重命名、移动、复制与恢复后要更新相关目录缓存。官方列表包含回收站记录，以 lastFileId=-1 表示结束，不能因过滤后的某一页为空而提前结束遍历。[列表文档](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/zrip9b0ye81zimv4)

### 3. 分享链接（优先，工作量较小至中等）

上传完成后生成分享链接，也可从列表多选文件创建分享。官方创建接口 POST /api/v1/share/create 支持 1/7/30 天或永久、提取码、最多 100 个文件 ID，并返回 shareKey；拼接链接还需要用户 UID。[创建分享](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/gzco1pi656ha792z)

后续可加入分享列表和使用情况。修改接口应严格按文档支持的字段实现，不能根据“修改分享”标题推断可改提取码、有效期或撤销分享。免登录提取流量相关开关应由用户明确选择。

### 4. 小文件单步上传（优先，工作量较小至中等）

获取上传域名后，使用 POST /upload/v2/file/single/create，单个文件上限 1GB；支持 containDir 和 completed 返回字段。适合许多小文件的上传，减少每个文件的协议交互，但域名获取仍是前置步骤，且速度提升应以实际测量为准。[单步上传](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/xhiht1uh3yp92pzc)、[获取域名](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/agn8lolktbqie7p9)

保留已有分片路径；其创建文件文档标注开发者单文件限制为 10GB，不要套用普通客户端会员宣传的上传上限。[分片创建](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/txow0iqviqsgotfl)

### 5. 容量与权益信息（低成本配套）

GET /api/v1/user/info 提供已用/永久/临时空间、临时空间到期时间、会员、剩余直链流量、开发者权益时间。可做首页容量条和批量导入前的提醒，但本地预估不能替代服务端最终判断。[用户信息](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/zgf9gyh7gvmdl4a3)

### 6. 直链管理（按需）

官方直链链接与当前 download_info/WebDAV 不是同一种能力。可增加直链空间启用、链接获取、缓存刷新及流量信息展示，适用于对外分发静态资源。[获取直链](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/tdxfsmtemp4gu4o2)、[刷新缓存](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/ptgqvx45rtaxry5v)

缓存刷新文档未列请求参数，不能擅自设计成服务端支持的“只刷新某个文件”。业务开通条件与实际额度仍需对应账户确认。

## 对前一份架构报告的补充

1. 文件复制在更新记录中标注为 2026-01-30 新增；SHA1 上传标注为 2026-01-23 新增，但仓库已经使用。恢复到指定目录和直链缓存刷新也在记录中。[更新记录](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/ewgaoswrngr1amb1)
2. 当前 _complete_upload 已检查 completed，问题不是这里完全没有检查，而是 JSON 导入和 batch_upload 没有可靠传递总体完成结果。SHA1 官方仅将 reuse=true 定义为成功，异常不等于“云端无此文件”。[上传完成](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/fzzc5o8gok517720)、[SHA1](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/de0et33ct3uhdfqs)
3. 新增离线下载和复制后，应由各任务模块隐藏任务编号、轮询与状态转换，调用者只处理明确的结果；不必先构建通用工作流框架。只有发现两者确实共享稳定行为时再抽象。
4. 当前接入流程要求开发者权益包；开发须知还说明同一 client_id 最多同时使用 3 个 token、token 有效期 30 天，以及各接口限流。批处理并发与多进程认证需要尊重这些条件。[接入流程](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/hpengmyg32blkbg8)、[开发须知](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/txgcvbfgh0gtuad5)

## 建议顺序

先修批处理完成判定 → 新增离线下载及进度 → 增加分享、容量信息 → 批量文件整理 → 小文件上传优化。若主要诉求是目录管理，则把批量整理提前。视频转码、图床、付费分享和第三方授权在官方目录中存在，但会明显扩大项目范围，暂不列为第一轮工作。
