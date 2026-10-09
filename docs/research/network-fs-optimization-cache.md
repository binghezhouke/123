# 网络文件系统与客户端缓存/一致性优化调研

调研日期：2026-10-09。目标是为网络文件系统客户端选择缓存层、TTL、写回策略和断线行为提供可落地参考。不同协议的一致性语义不同，不能只按“缓存越大越快”配置。

## 对比结论

| 客户端/协议 | 元数据缓存与 TTL | 数据读缓存 | 写回/持久化 | 一致性与断线要点 |
|---|---|---|---|---|
| Linux NFS | `acregmin`/`acregmax` 控制普通文件属性缓存（默认 3s/60s）；`acdirmin`/`acdirmax` 控制目录（30s/60s）；`actimeo` 统一设置；`noac` 禁用属性缓存 | 内核页缓存；NFSv3 WCC 用请求前后属性帮助发现变化 | close 时写回待处理修改；可用 `nocto` 关闭 close-to-open（通常不建议） | 默认 close-to-open（CTO）：open 时向服务器校验，close 时 flush；`noac` 增加 RPC、降低吞吐。网络中断期间 hard/soft 等挂载策略决定是否重试/返回错误 |
| SMB3/Windows | oplock/lease（含 directory lease）允许客户端缓存句柄、读、写和目录枚举；服务器变更通过 lease break 通知失效 | lease 允许读缓存，减少往返；目录 lease 延长目录元数据缓存 | SMB 3 写缓存可由 lease 授予；`/WRITETHROUGH` 或 `-UseWriteThrough` 要求写完成后落到存储栈 | lease break 维持跨客户端一致性；关闭 oplock/lease 会显著增加往返。写穿适用于需要提交确认的关键写入，代价是延迟 |
| rclone VFS | 重点是本地 VFS 文件缓存，不是远端 metadata TTL；缓存对象可按 `--vfs-cache-max-age` 清理，轮询间隔 `--vfs-cache-poll-interval`（默认 1m） | `full` 模式读写都落盘，稀疏文件按需下载；`--vfs-read-ahead` 预读、`--buffer-size` 内存缓冲 | `writes`/`full` 在 close 且空闲 `--vfs-write-back`（默认 5s）后上传；失败指数退避重试（最多约 1m）；进程退出后下次继续上传未完成文件 | off/minimal/writes/full 逐级提高 POSIX 兼容性和磁盘占用；断电前未上传数据依赖缓存目录。远端其他客户端修改不会因 write-back 自动立即可见，需考虑主动失效/低缓存 |
| JuiceFS | 客户端内存元数据缓存默认 5 分钟（`--metacacheto`），通过元数据变更订阅异步失效；默认 file `open` 绕过缓存实现 CTO；`--opencache` 会缓存 open，带来短暂陈旧窗口 | 内核 page cache + 客户端内存/本地盘 block cache；`--prefetch` 控制并行预取；可 `warmup` 预热 | FUSE `writeback_cache` 聚合小随机写；JuiceFS `--writeback` 先提交元数据、写本地 staging 后异步上传，适合大量小文件但有本地盘丢失风险；`--upload-delay` 可延迟上传 | 默认 close-to-open；异步事件失效意味着启用 `--opencache` 时跨客户端有微小延迟。缓存目录故障通常回退直读对象存储；writeback 下底层卡住可能把读写一起卡住，缓存盘不建议放网络文件系统 |
| ObjectiveFS | 内存缓存同时覆盖数据和元数据，`CACHESIZE` 默认按内存比例（大于 3GB 机器约 20%）；文档未公开固定 TTL，淘汰由客户端自动管理 | 可选内存缓存和压缩/加密/完整性校验的持久磁盘缓存；FUSE `kcache`（随 multithreading）减少重复读取开销 | 公开文档主要描述读缓存；磁盘缓存跨卸载保留、后台按最旧数据清理并保持空闲空间，不应当当作写入确认语义 | 磁盘缓存可复制预热或删除；一致性失效/离线写行为未在公开用户指南中承诺，应把对象存储可达性与挂载错误作为失败条件处理 |

## 官方资料中的关键行为

### NFS

Linux `nfs(5)` 明确指出，属性缓存可减少网络和服务器负载，但 `noac` 会让每个需要属性的操作都回源。默认 CTO 语义是：应用 `open` 时检查服务器存在性/权限，`close` 时写回待处理修改并报告写错误。NFSv3 的 WCC 通过单次请求前后的属性帮助客户端发现并发修改，但不能在大量异步并发写入时提供完美判断。

来源：[Linux nfs(5) 手册](https://man7.org/linux/man-pages/man5/nfs.5.html)。

### SMB3

Microsoft 的 SMB 特性说明称，SMB 3 的 oplock/lease 和 directory lease 可减少往返；目录缓存依靠服务器变更通知保持一致。SMB 3.1.1 的 write-through 可通过 `NET USE /WRITETHROUGH` 或 `New-SMBMapping -UseWriteThrough` 请求写入在返回成功前通过软硬件栈落盘。共享级 `LeasingMode=Shared` 只授予读缓存 lease，`None` 关闭 oplock/lease（官方不建议，除非兼容性需要）。

来源：[SMB features in Windows and Windows Server](https://learn.microsoft.com/en-us/windows-server/storage/file-server/smb-feature-descriptions?tabs=windows-server)、[Get-SmbShare LeasingMode](https://learn.microsoft.com/en-us/powershell/module/smbshare/get-smbshare)。

### rclone VFS

rclone VFS 文档定义四种模式：`off` 直接读写远端且不能可靠 seek/重试；`minimal` 仅对读写同时打开的文件做最小缓冲；`writes` 对写入文件落盘并在上传失败时指数重试；`full` 读写都落盘，按稀疏文件记录已下载区间。写回只发生在文件关闭且经过 `--vfs-write-back` 空闲时间后；缓存受 max-age、max-size、min-free-space 和轮询周期约束，但打开文件不能驱逐。

来源：[rclone VFS file caching](https://github.com/rclone/rclone/blob/master/vfs/vfs.md)。

### JuiceFS

JuiceFS 文档把缓存分为内核 page cache、客户端本地盘 cache 和进程 buffer。默认元数据缓存 5 分钟，并通过订阅元数据事件主动失效；文件 `open` 默认查询元数据服务以保持 CTO。`--opencache` 可把 open 也缓存，适合只读/读密集工作负载，但异步失效会造成小时间窗的跨客户端陈旧。FUSE `writeback_cache` 适合高频小随机写；JuiceFS `--writeback` 则把数据先写 staging 并异步上传，可能因缓存盘故障永久丢数据。

来源：[JuiceFS Cache（Cloud）](https://juicefs.com/docs/cloud/guide/cache/)、[JuiceFS Cache（Community）](https://juicefs.com/docs/community/guide/cache/)、[JuiceFS 命令参考](https://juicefs.com/docs/community/command_reference/)。

### ObjectiveFS

ObjectiveFS 用户指南公开了两层本地缓存：内存缓存覆盖数据和元数据（`CACHESIZE`），磁盘缓存通过 `DISKCACHE_SIZE`/`DISKCACHE_PATH` 启用。磁盘缓存压缩、加密并带完整性校验，卸载后保留，可用于快速重启；后台清理按最旧数据淘汰并保留配置的空闲空间。启用 multithreading 后，支持 FUSE kernel cache 以降低重复读取的 FUSE 开销。公开文档没有固定 TTL 或离线写入保证，因此不能据此推断断线可写。

来源：[ObjectiveFS User Guide：Memory Cache / Disk Cache / Kernel Cache](https://objectivefs.com/userguide)。

## 可实施建议

1. **按一致性等级选配置。** 多客户端共享且要求“写完后别人马上看到”时，保留 NFS CTO/SMB lease 通知/JuiceFS 默认 file-open 校验；避免 NFS `nocto`、JuiceFS `--opencache` 或长时间 VFS 本地缓存。只读发布、模型数据等可接受短暂陈旧时，再提高 TTL 或开启 open/目录缓存。
2. **把元数据 TTL 与数据缓存分开调。** 目录遍历密集工作负载先调目录/entry cache；随机读调小预读（JuiceFS `--prefetch=0` 或适中 `read-ahead`），顺序读再增大预读，避免读放大和缓存抖动。
3. **写回只用于可恢复数据。** rclone `writes/full`、JuiceFS `--writeback` 都会把成功写入与远端持久化解耦；为缓存目录使用本地 SSD、监控待上传字节和年龄，设置磁盘上限，并在卸载/重启流程中等待队列清空。关键数据使用 NFS close flush、SMB write-through 或显式 `fsync`/上传确认。
4. **断线策略要显式化。** NFS 生产共享优先 hard 挂载并设置合理超时，避免 soft 导致静默 I/O 错误；rclone/JuiceFS 写回需把“缓存盘可用”和“后台上传可达”作为健康指标，回退直连对象存储时记录告警；不要把 JuiceFS/ObjectiveFS 的缓存目录放在可能无限等待的网络文件系统上。
5. **验证缓存一致性。** 用两客户端测试：A 写入并 close，B 立即 open/read；目录 create/rename；并发 append/随机写；网络断开后读写、进程崩溃后恢复。记录 RPC/SMB round trips、缓存命中率、失效延迟、待上传队列和数据校验错误，再据真实工作负载调整 TTL 与缓存大小。
6. **利用协议原生通知而非轮询。** SMB lease break、JuiceFS 元数据订阅和 NFS 的 open/WCC 能在缓存较长时维持可接受一致性；若后端只能轮询（如对象存储挂载），应提供显式 `invalidate`/低 TTL 运维开关。
