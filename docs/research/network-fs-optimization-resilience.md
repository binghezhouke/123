# 网络文件系统优化与韧性调研

日期：2026-10-09  
范围：`mount123` 的 Linux 只读 FUSE 挂载；重点是目录遍历限流、负缓存、断线恢复、请求合并、直链复用、分层缓存和内容寻址。

## 结论

`mount123` 当前实现已经覆盖了大部分高价值路径：云端目录按目录 ID 共享不可变快照，过期目录后台刷新并在瞬态错误时继续提供旧快照；`storage.Cache.Acquire` 合并同一缓存键的并发填充；下载直链在缓存内按文件键合并解析并复用，失败时只淘汰实际失效的 URL；Range 数据按文件身份、大小和 HTTP 验证器进入持久缓存；有内存元数据层、磁盘对象层和内核 FUSE 属性/目录项缓存。

建议按以下顺序继续演进：

1. **P0：目录 API 的配额感知调度和可观测性。** 目前有构建并发上限和 API 重试，但还应将目录列举、单项详情、直链解析、Range 下载分别计数，记录 429/Retry-After、排队时间和每个父目录的请求速率；对同一账号/父目录实施令牌桶，避免 `find`/多进程同时遍历把 123 API 推入限流。
2. **P0：显式的短期负缓存。** 对已经确认不存在的云端名称、无效控制文件和永久失败的父目录查询做 1–5 秒内存负缓存，并在 `refresh`、目录快照替换、密码/探测结果发布时按父目录失效。负缓存必须只缓存确定的 `not_found`/永久错误，不能缓存超时、429、5xx 或断线。
3. **P1：断线恢复的分级退避和陈旧度策略。** 当前目录刷新已有最多 30 秒退避；Range 也有有限重试。建议统一 jittered exponential backoff，尊重 `Retry-After`，并把“旧目录仍可用”“缓存 Range 可读”“需要联网才能验证根/来源”分别暴露在 `io-stats`/`doctor` 中。
4. **P1：保持请求合并的键空间边界。** 继续按账户、API 地址、父目录 ID、File ID、内容版本/大小和 HTTP 验证器隔离；绝不能仅以路径或文件名合并。对相邻但不重叠的 Range 可增加短窗口合并，先通过指标验证读放大是否下降。
5. **P1：直链缓存安全与失效。** 当前 6 天、最多 4096 条的进程内直链缓存符合短期签名使用方式。建议增加“预计过期前刷新”和按 401/403/404/410 统计失效原因；继续禁止 URL 落盘和日志输出。不要把直链 TTL 与内容/索引 TTL 混为一谈。
6. **P2：内容寻址只作为无可靠版本信息时的降级方案。** 当前优先使用 API 内容版本、大小、强 ETag 或有效 Last-Modified；这是正确的成本/一致性折中。对于没有任何可靠验证器的来源，可选地对已读 Range 做块级摘要，或在完整成功后写入摘要索引，但不建议默认对大文件做全量 SHA-256。
7. **P2：分层缓存参数自适应。** 保留当前“内存元数据 → 持久 Range/索引 → HTTP Range”的层次；用命中率、重复淘汰、预取未消费字节和恢复命中率驱动调参。只有在指标证明热点重复读取时，才考虑额外共享缓存或内核页缓存。

## 官方项目的可迁移经验

### rclone VFS：目录 TTL、显式刷新和稀疏文件缓存

[rclone mount 文档](https://rclone.org/commands/rclone_mount/) 将目录项与数据缓存分开：`--dir-cache-time` 控制目录认为新鲜的时间，支持的后端可用 polling 更早发现变化，也可用 `SIGHUP` 刷新所有目录缓存。属性默认约 1 秒；文档指出设为 0 会导致内存增长、Samba 兼容性问题和目录列举变慢。其 `--vfs-cache-mode full` 使用稀疏文件，只把实际读过的区间写入磁盘，并通过 `--vfs-read-ahead` 进行磁盘预读。

对 `mount123` 的含义：24 小时云目录 TTL 配合显式 `refresh` 适合只读网盘；应保留“旧快照可服务、后台刷新”的行为，而不应把目录 TTL 设为 0 来追求强一致。`mount123` 的 Range 缓存已经比普通文件缓存更细粒度；调参应看实际命中率和预读消费率，而不是照搬固定预读大小。

### JuiceFS：内核元数据 TTL、负查找缓存和多级数据缓存

JuiceFS [缓存文档](https://github.com/juicedata/juicefs/blob/main/docs/en/guide/cache.md) 明确区分 FUSE 内核的 attribute、entry、directory-entry 和 negative-entry TTL；默认值约为 1 秒，负查找缓存用于重复的不存在项。它同时按“内核页缓存 → 客户端内存缓冲 → 本地磁盘缓存 → 对象存储”读取，并把下载块异步写入后续层；`--prefetch` 与顺序 `readahead` 分开，随机稀疏访问可关闭预取或只缓存局部块。磁盘缓存以大小和最小剩余空间约束，采用类似 LRU 的淘汰。

对 `mount123` 的含义：可以在 FUSE 层为稳定的 `ENOENT` 增加很短的负 dentry TTL，但目录刷新和隐藏控制文件发布必须主动失效，否则新出现的归档别名会被旧负项遮蔽。当前元数据预算和缓存保留等级符合 JuiceFS 的分层思路；建议增加“负缓存命中/失效”统计，并保持预取和前台 Range 共享总字节预算。

### Mountpoint for Amazon S3：限流退避、并行 Range 和本地/共享缓存

AWS 官方 [Mountpoint 配置文档](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md) 说明了几项与网盘 API 直接相关的策略：目录查找可能并发发出 `HeadObject` 和列表请求；遇到 S3 503 SlowDown 时最多重试 10 次并使用带抖动的指数退避；并行操作数、吞吐上限和读分片大小分别受控；本地缓存按最大尺寸或剩余空间淘汰 LRU 内容。它还支持本地缓存与跨实例共享缓存组合，以减少同一对象的重复请求，但强调共享缓存的权限、成本和缓存投毒风险。

对 `mount123` 的含义：目录 API、直链 API 和 Range API 应有独立的并发/速率预算，且要尊重 Retry-After；全局 32 个下载请求和 128 MiB 在途预算不应自动推导出同样的 API 列表并发。共享缓存不适合当前包含账户隔离和短期授权直链的场景；若将来增加共享层，键必须包含账户/API 端点和内容身份，且不得保存直链或密码。

### SeaweedFS：本地元数据订阅和目录遍历

SeaweedFS 的[架构说明](https://github.com/seaweedfs/seaweedfs/wiki/SeaweedFS_Architecture.pdf)描述了本地 metadata cache：缓存异步订阅 filer 的元数据变化，目录列举和遍历直接从本地读取；云端内容缓存与元数据缓存分开。其 filer API 也提供流式、分页的 `ListEntries`（[官方 protobuf](https://github.com/seaweedfs/seaweedfs/blob/master/weed/pb/filer.proto)）。

对 `mount123` 的含义：123 API 没有事件订阅时，持久化目录快照加显式刷新是合理替代。超大目录应保持分页和上限，避免一次把全目录装入内存；遍历限流应以“父目录快照构建任务”计数，而不是以每个 FUSE `Lookup` 计数。

### Go singleflight / 请求合并

Go 官方扩展库的 [`singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) 提供同一 key 的并发调用合并，只有一个调用执行，其余等待结果。`mount123` 已在缓存填充、直链解析、归档索引任务中实现等价的 flight 机制。后续扩展时要保留取消语义：等待者取消不应取消仍有其他等待者的共享任务；发起者取消后，应允许仍存活的等待者接管重试。

## 针对当前实现的风险与边界

- 目录快照是按父目录 ID 的不可变对象，已有目录句柄保持旧快照。这解决了 `readdir` 过程中列表变化导致的游标错乱，但意味着刷新后旧句柄不会立刻看到新别名；文档和 `doctor` 应继续明确这一点。
- FUSE 属性/目录项缓存当前为 1 秒。若引入负缓存，建议从 1 秒或更短开始，不要把 24 小时云目录 TTL 直接用于负 dentry。
- `storage.downloadLink` 的 6 天缓存只在进程内存在，重启后会重新取得直链；这是安全选择。持久化内容身份可以绕过重复探测，但不能恢复已过期的授权 URL。
- HTTP Range 断线后只有验证器、范围和长度一致才允许补齐缺口；任何“先交付后校验”的渐进读取都必须继续报告尾部校验失败，不能把部分字节标记为完整缓存。
- 内容寻址摘要不能替代服务端版本。相同大小、相同弱校验器或 API 元数据延迟时，客户端自行哈希既昂贵又不能证明当前云端对象没有变化。

## 建议的验收指标

在 `io-stats` 中增加或确认以下区间指标后，再调整参数：

| 指标 | 目的 | 触发动作 |
| --- | --- | --- |
| 每父目录列表请求数、排队时间、429 次数 | 识别 `find`/并发遍历造成的限流 | 降低目录构建并发或增加令牌桶等待 |
| 负缓存命中、失效、刷新后误命中 | 验证短 TTL 是否减少无效查询 | 若误命中非零，缩短 TTL 或扩大失效范围 |
| Range 合并等待人数、重叠字节、重复下载字节 | 判断请求合并是否有效 | 增大合并窗口或修正缓存键 |
| 直链解析次数、复用次数、401/403/404/410 | 判断 URL TTL 与失效策略 | 只对实际失效 URL 提前刷新 |
| 目录磁盘恢复、旧快照服务、恢复失败 | 判断断线时用户可用性 | 调整 TTL/退避，保留最近有效快照 |
| Range/索引/预取缓存命中与淘汰后重取 | 判断磁盘预算和保留等级 | 调整 `-cache-gib`、预取窗口和保留策略 |

## 参考资料

- [rclone mount / VFS 文档](https://rclone.org/commands/rclone_mount/)
- [JuiceFS Cache 文档](https://github.com/juicedata/juicefs/blob/main/docs/en/guide/cache.md)
- [AWS Mountpoint for Amazon S3 配置文档](https://github.com/awslabs/mountpoint-s3/blob/main/doc/CONFIGURATION.md)
- [SeaweedFS Architecture](https://github.com/seaweedfs/seaweedfs/wiki/SeaweedFS_Architecture.pdf)
- [SeaweedFS filer protobuf API](https://github.com/seaweedfs/seaweedfs/blob/master/weed/pb/filer.proto)
- [Go `singleflight` 文档](https://pkg.go.dev/golang.org/x/sync/singleflight)
