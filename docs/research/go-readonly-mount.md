# Go 只读 123 网盘挂载与 ZIP 虚拟目录调研

调研日期：2026-10-07。范围是独立 Go 程序：只读挂载 123 网盘目录，并可把远端 ZIP 展示成目录；需要用户可配置的磁盘缓存。结论是应以 Go 标准库 `archive/zip`、go-fuse v2 和一个自己实现的有界磁盘块缓存组合实现。上游提供的是 FUSE 构建基础和 ZIP 示例，不是现成的 123 网盘挂载程序。

## 推荐

首选 Linux，采用 `github.com/hanwen/go-fuse/v2`，当前核实版本 `v2.11.0`，许可证 BSD-3-Clause。使用新式 `fs` API 定义只读节点；不选已标记 deprecated 的 `nodefs` / `pathfs`。v2.11.0 的模块要求 Go 1.21；项目依赖版本应固定到该标签，而不是跟随 `master`。[go-fuse 包版本与许可证](https://pkg.go.dev/github.com/hanwen/go-fuse/v2)；[go.mod](https://github.com/hanwen/go-fuse/blob/master/go.mod)

远端内容层建议拆为两类读取器：普通云盘文件使用带 Range 支持检测和回退的远端 `ReaderAt`；ZIP 则用同一类 `ReaderAt` 交给 `zip.NewReader(readerAt, size)`。由项目实现固定块大小、LRU 淘汰、缓存目录、最大磁盘占用、读穿和写入原子性的磁盘块缓存，并以云端文件 ID + 内容版本/大小 + 块偏移为键。ZIP 中央目录可随机访问；成员内容由 `zip.File.Open()` 解压读取。若源站不支持 HTTP Range，回退策略应是下载整个 ZIP 到缓存后再挂载，并让用户通过配置上限控制这一行为。

可把 [`github.com/snabb/httpreaderat`](https://github.com/snabb/httpreaderat)（核实版本 `v1.0.4`，MIT）作为 Range `ReaderAt` 的实现候选。它明确支持用 HTTP Range 配合 Go `archive/zip` 读取远端 ZIP，且文档推荐在外部增加缓冲层以降低小请求数；这并不等同于所需的可配置持久磁盘缓存，仍需自己实现/接入磁盘缓存、认证请求、链接刷新、Range 不支持时的策略和并发合并。[包版本与许可证](https://pkg.go.dev/github.com/snabb/httpreaderat)；[上游使用说明](https://github.com/snabb/httpreaderat#readme)

## 仓库已有语义

目前 Python API 客户端使用 `https://open-api.123pan.com`，获取分页目录走 `/api/v2/file/list`，下载信息走 `/api/v1/file/download_info`；下载 URL 的取得和 WebDAV URL 回退封装在 `FileService.get_final_download_url`。这些接口层可以作为 Go 端行为参考，但现有 Python 模块不能被 Go 直接复用。[api/client.py](../../api/client.py)；[api/file_service.py](../../api/file_service.py)。目录列表会按 `lastFileId` 分页，并过滤 `trashed == 1`；FUSE 的 `Readdir` 应完整翻页，同时避免目录项重复或分页遗漏。[api/file_service.py](../../api/file_service.py)

仓库现有 ZIP 预览代码已经体现了几个远端 ZIP 必需的防护：校验 Range 响应和总大小、校验 ETag/Last-Modified、识别链接过期/文件变化、拒绝分卷 ZIP，并限制索引大小及条目数。[api/zip_preview.py](../../api/zip_preview.py)。它的 Range reader 与归档索引缓存只服务当前 Python 预览流程；`ArchiveCache` 是进程内有界缓存，不是可供 FUSE 复用的通用持久化磁盘块缓存。[api/archive_cache.py](../../api/archive_cache.py)

123 开放平台对外提供的开发者文档入口可见于 [123 云盘官方文档空间](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced)，但本次抓取工具无法打开该页面正文。可读的接口细节副本记载了 `open-api.123pan.com`、`/api/v1/access_token`、文件列表和下载相关接口；需注意该副本的 GitHub 发布者是 `25class`，不是 123 官方组织。[123Open-Docs 开发者接入文档](https://github.com/25class/123Open-Docs/blob/main/%E5%BC%80%E5%8F%91%E8%80%85%E6%8E%A5%E5%85%A5.md)。正式接口细节应以官方文档/控制台现行内容和实际响应为准。官方 OpenAPI 的 `downloadUrl` 是否稳定支持 HTTP Range，本次没有找到其官方保证；必须运行时检查 `206`、`Content-Range`、实体标识，并对不支持 Range 的响应安全回退，不能把 Range 支持当作 API 契约。

## ZIP 与 go-fuse 的适配边界

Go 标准库 `archive/zip.Reader` 接受 `io.ReaderAt` 和 ZIP 总字节数，故天然适合通过 Range 读取 ZIP 中央目录，而不必预先下载整个 ZIP。内建支持 Store 与 Deflate；不支持分卷 ZIP。`zip.File.Open` 可并发读取成员并校验 CRC；ZIP 不安全路径可能产生 `ErrInsecurePath`，挂载时应自行规范化并拒绝绝对路径、`..`、反斜线、重复文件/目录冲突以及越出根节点的名字，不能直接把压缩包路径拼成主机路径。[Go `archive/zip` 文档](https://pkg.go.dev/archive/zip)；[Go ZIP reader 源码](https://github.com/golang/go/blob/master/src/archive/zip/reader.go)

go-fuse README 包含可读 ZIP/TAR 的 `zipfs` 示例，但它不是目标实现：当前 `zipfs` 用 `zip.OpenReader` 打开本地归档，`Open` 时对整个成员 `io.ReadAll` 并把内容保存在内存中；因此既没有远端 `ReaderAt` 接口，也没有磁盘缓存/淘汰策略。应将它当作 FUSE 节点组织的参考，自行实现按偏移读取并可落盘的文件 handle。该示例使用新版 `fs` API；`nodefs` 与 `pathfs` 在项目文档中明确 deprecated，不应误将 ZIP 示例当成旧 nodefs API 示例。[go-fuse zipfs 源码](https://github.com/hanwen/go-fuse/blob/master/zipfs/zipfs.go)；[go-fuse API 状态说明](https://pkg.go.dev/github.com/hanwen/go-fuse/v2)

ZIP 成员若为 Deflate，无法像 Store 成员一样把 ZIP 内压缩字节位置直接映射成解压文件任意偏移。首个可行版本可在第一次打开成员时流式解压到磁盘缓存文件，之后按偏移读；Store 文件可直接映射压缩数据区并按块读取。无论哪种路径都应在 EOF 验证 CRC，并用归档版本校验防止缓存混合不同版本的数据。加密 ZIP、非 Store/Deflate 算法及分卷 ZIP 应明确返回不支持，除非另行加入实现和兼容测试；Go 标准库默认解压器并未提供通用加密 ZIP 支持。[Go ZIP 标准库](https://pkg.go.dev/archive/zip)

## FUSE 平台与运行环境

Linux 是首发平台。此环境运行在 Linux，`/dev/fuse` 当前存在（字符设备 major/minor `10:229`），但这只说明当前容器/主机暴露了设备；容器权限、mount namespace、seccomp/AppArmor 仍可能阻止实际挂载，部署验收时必须确认运行用户能访问 FUSE 且有 mount 权限。

macOS 可作为后续可选目标：go-fuse 上游明确表示主维护者没有 Mac 环境测试，并指出 OSXFUSE 缺少 NOTIFY、系统持续发起 STATFS、并发读取 FUSE 设备存在性能问题。因此将 macOS 标为尽力支持并单独验证，不与 Linux 首发承诺等同。[go-fuse macOS 支持说明](https://pkg.go.dev/github.com/hanwen/go-fuse/v2#hdr-macOS_Support)。

## 替代方案与成本

| 方案 | 可复用内容 | 许可证/版本 | 与目标的差距 |
| --- | --- | --- | --- |
| go-fuse v2 + Go 标准库 ZIP | FUSE 协议与新式节点接口；ZIP 目录与 Store/Deflate 解码 | go-fuse v2.11.0，BSD-3-Clause；Go stdlib，BSD-3-Clause | 自行实现 123 API 客户端适配、远端 ReaderAt、只读节点、磁盘缓存、缓存失效和挂载命令。推荐。 |
| snabb/httpreaderat | 远端 HTTP Range 到 `io.ReaderAt` 的候选实现 | v1.0.4，MIT | 没有 123 授权/令牌刷新、持久磁盘块缓存、FUSE 节点、软链接和实体变化策略；项目文档建议另加 buffer reader。 |
| OpenList `123_open` driver | 123 OpenAPI 鉴权、分页、下载链接等 API 语义参考 | OpenList v4 代码，AGPL-3.0 项目 | 它是 OpenList 插件接口下的存储 driver，不是可独立嵌入的最小客户端；集成会引入 OpenList 内部模块/许可证审查成本，且 driver 有写操作，不是只读 FUSE。可参考 API 行为，避免直接依赖整个项目。[driver 源码](https://github.com/OpenListTeam/OpenList/blob/main/drivers/123_open/driver.go)；[项目许可证](https://github.com/OpenListTeam/OpenList/blob/main/LICENSE) |
| rclone-123pan | 功能较全的 123pan rclone 后端，支持 Range/Seek、VFS、serve | 项目 MIT；当前主分支 Go 1.25，依赖 rclone v1.75 | 该项目是 rclone 树外集成/定制发行版，代码和依赖规模远大于独立只读挂载；其文档说明 API 行为参考 OpenList driver 和网页客户端。作为现成产品/行为对照值得评估，不能称为现成 Go FUSE ZIP mount。[仓库说明](https://github.com/Ljzd-PRO/rclone-123pan)；[go.mod](https://github.com/Ljzd-PRO/rclone-123pan/blob/main/go.mod) |

## 结论与实现顺序

先做 Linux 只读挂载，Go 1.21+（推荐项目采用当前维护工具链），固定 go-fuse `v2.11.0`。先实现 Python 现有 API 的认证、目录分页、下载 URL 解析，再实现普通文件的可寻址远端读取和有界磁盘块缓存。接着让 ZIP 根目录使用相同 ReaderAt；Store 成员采用压缩数据范围读取，Deflate 成员首次访问时解压到受配额管理的缓存文件。对 Range 缺失/实体变化/URL 过期定义明确的下载回退和缓存失效行为。最后评估 macOS 独立适配。

这里的 Go-FUSE zipfs、snabb/httpreaderat、OpenList 和 rclone-123pan 都只是可以借鉴的组件/实现；没有发现一个可直接安装、同时满足 123 OpenAPI 只读挂载、ZIP 虚拟目录和配置化磁盘缓存的上游成品。

## 首版落地决策

实现位于 [`mount123/`](../../mount123/README.md)，选择 go-fuse v2.11.0 与标准库 ZIP，自行实现块缓存和 Range 读取。首版对不支持 Range 的下载源直接报错，暂不加入整包下载回退；Store 随机读取不保证整成员 CRC 校验，Deflate 解压到缓存后验证 CRC。实际支持范围与运行方式以工具 README 为准。
