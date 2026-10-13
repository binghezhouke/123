# sevenzip 维护分支

挂载器使用 [binghezhouke/sevenzip](https://github.com/binghezhouke/sevenzip)，上游是 [bodgit/sevenzip](https://github.com/bodgit/sevenzip)，许可证为 BSD-3-Clause。

当前依赖的精确伪版本以 [`mount123/go.mod`](../mount123/go.mod) 中的 `replace` 为准，基于上游 `v1.6.1`。[`mount123` 维护分支](https://github.com/binghezhouke/sevenzip/tree/mount123) 从上游提交 `a40a39ef0f29542db3d47d1dcb58d011b214feb8` 开始维护，不直接跟随上游 `main` 升级。`replace` 指向 fork，保留原模块及 import 路径，避免修改库的内部引用。

## 扩展边界

[问题 #42](https://github.com/binghezhouke/123/issues/42) 跟踪固实 7z 读取优化。现有库的 `File.Stream` 已提供分组编号，同一 Reader 内也有解码器池；基础 Reader 复用可以由挂载器实现。逐成员新建 Reader 则无法共享该池。

fork 已提供：

- `Reader.Streams()`：各组编号、解压后大小及 `Stored` 标志，调用本身不打开解码器。`Stored` 只在单一无属性 Copy 编码器、单输入输出、无绑定且长度一致时成立；压缩前后大小相等不能单独证明可直接映射（例如 Delta + Copy）。
- `File.StreamOffset()`：成员在组内的解压偏移，并区分没有数据流的目录和空文件。
- `File.Checksum()`：直接返回解析好的成员 CRC 及是否声明；缺失 CRC 与显式零值不同。查询不扫描其他成员、不读来源、不创建解码器。
- `Reader.OpenStream(index)`：独立拥有的组级解码流；读取到 EOF 检查长度和已声明的组 CRC（包含零值），显式 Close 直接释放解码器，不进入 `File.Open` 的池。
- `Reader.PackedRanges(index)`：所选组各压缩输入在原始 ReaderAt 上的绝对区间，包含归档前缀和起始头，校验长度与溢出。
- `Reader.OpenStreamWithReader(index, readerAt)`：保留已解析头部，替换所选组正文来源；调用者始终拥有来源，构造失败也由调用者清理。
- `Reader.StreamLayout(index)`：复制组布局、成员偏移及明确的 CRC presence（包含值为零的 CRC），不创建解码器或保留 Reader、来源、密码。`PlainLZMA2DecoderInfo` 提供内置普通 LZMA2 的字典和缓冲事实，用于应用层决定是否保留状态；不覆盖自定义 codec 注册。
- `StreamLayout.AESLZMA2DecoderInfo()`：识别单输入 AES→LZMA2 串联，验证属性及填充长度，不读正文、不派生密钥。额外驻留预算为 256 KiB；不包含 KDF CPU 时间或库原有的全局密钥缓存。
- 组级正文使用 1 MiB 输入缓冲，减少网络 ReaderAt 调用；头解析及原 File.Open 缓冲不变。

内外层成员的[按需共享组游标](design/nested-solid7z-continuation.md)使用上述复制元数据与独占流接口，保留有界进程内状态。符合准入条件的顶层组超过 1 MiB 时也按需读取；已有完整成员或组缓存优先复用。其余组保留下述完整组缓存策略。

来源身份、密码身份、并发调度、取消策略、缓存预算及持久化由挂载器负责。每次组填充创建一个 Reader，直接将解压流写到共享的 growing 缓存，不跨填充保留解码器。组缓存按账户、内容版本、分卷身份、密码身份和组编号隔离，成员位置随索引持久化；旧 7z 索引缺少位置时重建，不使其他格式的索引失效。发布前也检查组内可用的成员 CRC。

组缓存大小上限为 `min(1 GiB, 总缓存容量 / 4)`，与成员缓存和 Range 数据共用总磁盘预算。小规模组在短读关闭后继续完成填充，最长 5 分钟，卸载或缓存关闭会取消；更大的组或组空间预留失败时回退原有成员解压。实际组解压受现有构建槽限制，等待组内容的成员不占构建槽。已完成组跨重启复用。固实组首次冷读仍需要从组头解码到目标位置。

[#43](https://github.com/binghezhouke/123/issues/43) 的窗口策略位于挂载器：每输入最多当前与下一窗口、单窗口最大 8 MiB、组内输入合计最多 16 MiB，缓存小时缩小。窗口在小读之前登记 exact Range，复用 disk staging 渐进供数；消费半窗口时可启动后续后台窗口。已消费窗口持有所有实际下载引用并检查最终 HTTP 结果，取消释放引用。分卷仅在初始化来源时同步，不持网络读锁。库不负责预读策略或任务生命周期。

## 更新方式

在 fork 的 `mount123` 分支提交并测试最小必要改动，保留上游许可证。挂载器只引用已推送的明确标签或提交对应的 Go 伪版本；不使用本地绝对路径替换，也不依赖浮动分支。升级后运行 Go 全套测试、FUSE 集成测试、竞态检查与 vet，并针对短读、同组顺序读取、随机跳转及温缓存验证行为。

上游库普通全套测试通过；本次 fork 的全量竞态测试在 `TestFS` 达到 600 秒超时；仅排除 `TestFS` 后，其余原生测试和新增接口测试全部通过。未把 `TestFS` 记为通过，也未对旧版本作同预算耗时对照。挂载器单独运行全量竞态与 FUSE 集成测试。
