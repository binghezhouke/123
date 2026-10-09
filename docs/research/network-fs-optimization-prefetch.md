# 网络文件系统的预取、并发与随机读取优化

调研日期：2026-10-09。目标是为当前 HTTP/对象存储后端的只读 FUSE 挂载提取可迁移的设计原则。这里的“网络文件系统”包括 Linux NFS、Linux CIFS/SMB、rclone VFS，以及 S3FS 和 goofys；协议租约、写回和 POSIX 一致性语义不视为本项目的功能需求。

## 资料与可验证事实

### NFS/Linux netfs

- Linux NFS 的 `nfs_readahead()` 将内核页缓存的 readahead 请求交给 NFS 的 pageio 聚合层；实现会把多个 folio 加入批次后提交，而不是为每个页单独发 RPC。[Linux NFS `fs/nfs/read.c`](https://github.com/torvalds/linux/blob/master/fs/nfs/read.c)
- Linux 的通用 netfs 接口把一次读拆成可分别命中缓存或远端的子请求，并提供 `prepare_read`、`query_occupancy`、`netfs_readahead` 等钩子。[Linux `include/linux/netfs.h`](https://github.com/torvalds/linux/blob/master/include/linux/netfs.h)
- NFS 可选 FS-Cache，把数据落到本地磁盘；这属于内核缓存层，不是用户态 Go 程序可以直接复用的通用缓存库。[NFS Kconfig 的 `NFS_FSCACHE`](https://github.com/torvalds/linux/blob/master/fs/nfs/Kconfig)

可迁移的部分是“拆分读请求、聚合相邻范围、允许缓存命中与网络子请求并行完成”。NFS 的 delegation、hard mount 无限重试和服务器状态恢复依赖 NFS 服务器，不能作为 HTTP Range 的默认语义。

### SMB/CIFS

- SMB2/3 的请求有 CreditCharge；在 2.1 及以后方言中，发送方用它表示该请求消耗的 credit，服务端通过 CreditResponse 授予额度。[MS-SMB2 packet header](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fb188936-5050-48d3-b350-dc43059638a4)
- SMB READ 是可异步处理的独立请求，协议示例展示了按 offset/length 读取并用 credit 控制在途工作。[MS-SMB2 reading a remote file](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/4801c3e5-dd73-4e1c-b2ba-8ebf73642227)
- Lease 的 READ_CACHING 允许客户端缓存数据；服务端发送 lease break 时，客户端必须清理或刷新受影响的缓存。[MS-SMB2 lease break](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/4f35576a-6f3b-40f0-a832-1c30b0afccb3)

可迁移的部分是“请求成本与并发额度绑定，并在发出请求前做准入”。不要把 SMB credit 数字直接当作 HTTP 带宽或内存上限，也不要在没有服务器召回协议的对象存储上伪造 lease 失效通知。

### rclone VFS

rclone 文档把三个旋钮明确分开：

1. `--buffer-size` 是每个打开文件的内存缓冲上限，且不与其他句柄共享；总内存可能接近“每句柄上限 × 打开句柄数”。
2. `--vfs-read-ahead`（仅 full cache 模式）是落盘的额外前读量；读取时总前读为 buffer 加 read-ahead。
3. chunk size 决定单个远端范围，chunk streams 决定并发范围数。streams=0 时块逐次倍增；streams>0 时固定块并行。官方给出的高延迟或高性能 S3 起点示例是 4 MiB × 16 streams，并强调需要按后端和链路实测。[rclone mount VFS 文档](https://rclone.org/commands/rclone_mount/#vfs-chunked-reading)

rclone 还提供 `--vfs-read-wait`：无磁盘缓存时，乱序读到来后会短暂等待顺序读，避免立即 seek。这个机制适用于“调用顺序略乱但仍接近顺序”的程序，不适合真正随机访问。[rclone performance options](https://rclone.org/commands/rclone_mount/#vfs-performance)

### S3FS 与 goofys

- s3fs 支持可选本地文件缓存、内存元数据缓存和最多 `max_thread_count` 个并行请求；其手册明确建议为同时打开的读写文件准备临时/缓存空间。[s3fs 手册](https://github.com/s3fs-fuse/s3fs-fuse/blob/master/doc/man/s3fs.1.in)
- s3fs README 说明 S3 的目录元数据操作受网络延迟影响，且不同客户端之间没有协调；这意味着只读挂载仍需要版本/失效策略，不能把目录缓存当成永远有效。[s3fs README](https://github.com/s3fs-fuse/s3fs-fuse)
- goofys 明确选择性能优先，不提供内置磁盘数据缓存（可配合 catfs），并对随机写等需要多次往返的 POSIX 行为作出限制；其 close-to-open 一致性模型也不等同于不可变快照。[goofys README](https://github.com/kahing/goofys)

两者共同说明：对象存储应优先做范围读取和有限缓存，不能假设完整文件下载或 POSIX 目录语义；并发线程数必须和本地磁盘、连接数及请求大小一起限额。

## 面向当前只读挂载的建议

### 1. 将三个数量分开记账

现有下载调度应继续区分：

- 网络请求尚未结束的响应字节；
- 下载中暂存、等待发布的字节；
- 已消费和仅推测预取的缓存字节。

SMB credit 的启发是“发请求前预留成本”，rclone 的启发是“块大小和并发流数独立调节”。因此全局调度应同时限制请求数和预计响应字节，并保留前台配额；不能只限制 goroutine 数，也不能把字节预算解释成带宽整形器。

### 2. 以消费反馈控制顺序预读

每个句柄跟踪消费位置、连续就绪前沿、在途范围和最近一次真正等待。连续读且前台等待增加时，逐步扩大窗口或块大小；随机 seek、低命中、后台排队或缓存盘拥塞时收缩。预读只有在消费接近前沿时续填，并设置“已完成但未消费”的上限，避免快网络把慢消费者的磁盘塞满。

重叠或轻微乱序的 FUSE Read 不应自动取消预测代次；只有超过邻近距离的 seek 才重置。可采用类似 rclone `read-wait` 的很短等待，但要以 P95 前台延迟为上限，并在真正随机访问时快速放弃等待。

### 3. 让前台读加入并提升已有 flight

前台读取命中一个后台 Range 时，应加入同一共享任务并提升后续子请求的优先级；不要重复发起请求。HTTP 已发出的请求无法抢占，所以优先级只影响队列中的下一段。取消一个读者只能减少引用；仍有前台读者时不得取消共享下载。

下载中范围应允许前台读取已经写入暂存文件的前缀，同时延后完整长度、校验和、版本验证及原子缓存发布。这样可以降低大预读块造成的首读延迟，又不把不完整响应标记为完整缓存。

### 4. 随机读取使用稀疏范围缓存

对大对象采用固定或上限递增的 Range 块：小块降低随机读放大，大块减少高 RTT 下的请求数。块大小与并发数独立配置，并以“实际消费字节 / 下载字节”评估预取收益。只读挂载可使用稀疏暂存文件记录已填范围；不应为了少量随机读下载整个对象。

预取缓存应进入低优先级淘汰队列；一旦被前台命中再提升为热点。目录索引、版本元数据和已消费正文需要有限保护额度，避免一次大对象顺序扫描淘汰所有小文件热点。

### 5. 一致性与只读边界

当前挂载的版本条件、Content-Range、长度和末尾校验必须在发布前验证。对象存储没有 SMB lease break；当远端版本改变时，使用实体标识/版本号隔离旧 flight 和新缓存，不能拼接两个版本的范围。只读只禁止写操作，不代表远端命名空间不会被其他客户端修改，因此目录快照仍需明确 TTL 或版本边界。

## 不建议直接移植

- NFS delegation、SMB lease/oplock：它们依赖服务器授予与召回，HTTP/S3 没有等价的回调。
- NFS hard mount 的无限重试：交互式只读浏览需要有界重试、取消和超时。
- FS-Cache、页缓存参数的整套内核实现：当前 FUSE 使用用户态范围缓存；重复叠加内核和用户态缓存会增加内存与失效复杂度。
- goofys/s3fs 的 POSIX 假设：两者都承认对象存储目录和随机更新语义有限，不能作为当前版本一致性契约。

## 建议的验证矩阵

使用可控 Range 服务分别施加 5/50/150 ms RTT、50/250 MB/s 限速，比较冷缓存、进程内热缓存和重挂载热缓存：

| 场景 | 重点指标 |
| --- | --- |
| 单文件顺序读 | 前台 P50/P95/P99、有效吞吐、预读命中率 |
| 慢消费者 + 快网络 | 已消费前沿后的就绪/在途字节上限、磁盘占用 |
| 4/16 个文件混读 | 前台公平性、后台饥饿、请求数与在途字节 |
| 稀疏随机 seek | 请求放大、重复下载、seek 后恢复时间 |
| 版本切换/断流 | 旧 flight 隔离、前缀提前交付后的错误传播 |
| 大目录与归档扫描 | 元数据请求数、索引缓存留存、其他文件 P95 |

应记录 HTTP 首字节、排队、传输、暂存写入、fsync/发布和 FUSE 返回的独立耗时；仅看总吞吐无法判断预取是否浪费。
