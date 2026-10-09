# sevenzip 维护分支

挂载器使用 [binghezhouke/sevenzip](https://github.com/binghezhouke/sevenzip)，上游是 [bodgit/sevenzip](https://github.com/bodgit/sevenzip)，许可证为 BSD-3-Clause。

当前依赖固定为 fork 的 `v1.6.2-0.20261009012119-5f9a0e73a37f`，基于上游 `v1.6.1`。[`mount123` 维护分支](https://github.com/binghezhouke/sevenzip/tree/mount123) 从上游提交 `a40a39ef0f29542db3d47d1dcb58d011b214feb8` 开始维护，不直接跟随上游 `main` 升级。`mount123/go.mod` 使用 `replace` 指向 fork，保留原模块及 import 路径，避免修改库的内部引用。

## 扩展边界

[问题 #42](https://github.com/binghezhouke/123/issues/42) 跟踪固实 7z 读取优化。现有库的 `File.Stream` 已提供分组编号，同一 Reader 内也有解码器池；基础 Reader 复用可以由挂载器实现。逐成员新建 Reader 则无法共享该池。

fork 已提供：

- `Reader.Streams()`：各组编号与解压后大小，调用本身不打开解码器。
- `File.StreamOffset()`：成员在组内的解压偏移，并区分没有数据流的目录和空文件。
- `Reader.OpenStream(index)`：独立拥有的组级解码流；读取到 EOF 检查长度和可用的非零组 CRC，显式 Close 直接释放解码器，不进入 `File.Open` 的池。
- 组级正文使用 1 MiB 输入缓冲，减少网络 ReaderAt 调用；头解析及原 File.Open 缓冲不变。

来源身份、密码身份、并发调度、取消策略、缓存预算及持久化由挂载器负责。每次组填充创建一个 Reader，直接将解压流写到共享的 growing 缓存，不跨填充保留解码器。组缓存按账户、内容版本、分卷身份、密码身份和组编号隔离，成员位置随索引持久化；旧 7z 索引缺少位置时重建，不使其他格式的索引失效。发布前也检查组内可用的成员 CRC。

组缓存大小上限为 `min(1 GiB, 总缓存容量 / 4)`，与成员缓存和 Range 数据共用总磁盘预算。小规模组在短读关闭后继续完成填充，最长 5 分钟，卸载或缓存关闭会取消；更大的组或组空间预留失败时回退原有成员解压。实际组解压受现有构建槽限制，等待组内容的成员不占构建槽。已完成组跨重启复用。固实组首次冷读仍需要从组头解码到目标位置。

## 更新方式

在 fork 的 `mount123` 分支提交并测试最小必要改动，保留上游许可证。挂载器只引用已推送的明确标签或提交对应的 Go 伪版本；不使用本地绝对路径替换，也不依赖浮动分支。升级后运行 Go 全套测试、FUSE 集成测试、竞态检查与 vet，并针对短读、同组顺序读取、随机跳转及温缓存验证行为。

上游库普通全套测试通过；竞态测试中原有 `TestFS` 压力测试超过 120 秒，其他测试及新增组接口测试通过。挂载器单独运行全量竞态与 FUSE 集成测试。
