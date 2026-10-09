# sevenzip 维护分支

挂载器使用 [binghezhouke/sevenzip](https://github.com/binghezhouke/sevenzip)，上游是 [bodgit/sevenzip](https://github.com/bodgit/sevenzip)，许可证为 BSD-3-Clause。

当前依赖固定为 fork 的 `v1.6.1`，对应上游提交 `a40a39ef0f29542db3d47d1dcb58d011b214feb8`，尚无解码行为变更。[`mount123` 维护分支](https://github.com/binghezhouke/sevenzip/tree/mount123) 已推送，从该提交开始维护；不直接跟随上游 `main` 升级。`mount123/go.mod` 使用 `replace` 指向 fork，保留原模块及 import 路径，避免修改库的内部引用。

## 扩展边界

[问题 #42](https://github.com/binghezhouke/123/issues/42) 跟踪固实 7z 读取优化。现有库的 `File.Stream` 已提供分组编号，同一 Reader 内也有解码器池；基础 Reader 复用可以由挂载器实现。逐成员新建 Reader 则无法共享该池。

下一步组级缓存需要评估在 fork 中提供：

- 固实组大小及成员在组内的解压偏移，供挂载器规划读取和预取。
- 从固实组起点打开解压流的接口，供多个成员共享一次解压结果。
- 明确的解码器关闭和释放接口，避免缓存淘汰后仍保留字典和来源读取器。

这些接口尚未实现。来源身份、密码身份、并发调度、取消策略、缓存预算及持久化仍由挂载器负责。共享解码器必须使用合适的生命周期，不能直接保留绑定于某次已取消读取请求的 Reader。

## 更新方式

在 fork 的 `mount123` 分支提交并测试最小必要改动，保留上游许可证。挂载器只引用已推送的明确标签或提交对应的 Go 伪版本；不使用本地绝对路径替换，也不依赖浮动分支。升级后运行 Go 全套测试、FUSE 集成测试、竞态检查与 vet，并针对短读、同组顺序读取、随机跳转及温缓存验证行为。

当前切换仅准备依赖维护，不代表 #42 已完成，也没有改变正在运行的挂载进程。
