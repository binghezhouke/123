# mount123：Linux 只读网盘挂载

独立 Go 命令，通过 123 开放平台 API 获取目录和下载直链，不经过 WebDAV，也不依赖 Python 网页服务启动。使用 [go-fuse v2](https://github.com/hanwen/go-fuse) 与 Go 标准库 [archive/zip](https://pkg.go.dev/archive/zip)。选型对比见[调研文档](../docs/research/go-readonly-mount.md)。

## 构建和运行

需要 Linux、Go 1.23 或更新版本、FUSE（例如 Debian/Ubuntu 的 `fuse3`），以及可访问的 `/dev/fuse`。使用普通用户运行。容器需要额外开放 FUSE 设备与挂载权限。

```bash
cd mount123
go build -o mount123 ./cmd/mount123
mkdir -p "$HOME/mnt/123"
./mount123 \
  -config ../config.json \
  -mountpoint "$HOME/mnt/123" \
  -cache-dir "$HOME/.cache/mount123" \
  -cache-gib 20 \
  -root-id 0
```

`config.json` 沿用网页服务的 `CLIENT_ID`、`CLIENT_SECRET` 字段。也可通过环境变量 `PAN123_ACCESS_TOKEN` 提供现成令牌；仅提供令牌时不强制要求配置文件存在，令牌过期后需要自行更换。应用凭据方式会自动申请、缓存和刷新令牌。缓存按应用 ID 与 API 地址隔离，不复用旧 Python 客户端未标记账户的令牌文件。

程序前台运行，Ctrl+C / SIGTERM 会卸载；也可以从另一终端执行：

```bash
fusermount3 -u "$HOME/mnt/123"
```

挂载点必须已存在且为空。缓存必须放在挂载点之外；同一缓存目录仅允许一个挂载进程使用。默认只对当前用户开放挂载，不开启 `allow_other`。

## 文件如何呈现

```text
~/mnt/123/
├── 普通目录/
│   └── 视频.mp4
└── 相册.zip/             # 保留 ZIP 文件名，但作为目录进入
    └── images/
        └── 001.jpg
```

`-zip-dirs=false` 可关闭映射，把 ZIP 当作普通文件读取。目录列表完整翻页，缓存 30 秒；FUSE 属性与目录项缓存 1 秒。挂载通过内核 `ro` 选项拒绝写入、创建、重命名、删除，节点也拒绝写方式打开及属性修改；API 客户端没有远端写入操作。

## 读取和缓存

- 普通文件：1 MiB 分块读取，使用 API 下载直链的 HTTP Range，按需缓存。直链在读取器内复用，过期或返回 401/403/404/410 时重新获取一次。
- ZIP 索引：读取中央目录，进入目录时不预先下载整个 ZIP；每个活动 ZIP 节点复用已解析索引。
- Store 成员：直接定位 ZIP 数据区，支持随机读取，无需完整解压。
- Deflate 成员：第一次打开时流式解压到磁盘，并完成 CRC 校验后返回文件句柄；后续在缓存中随机读取。大成员首次打开会等待，不是边解压边返回文件内容。
- 缓存默认 20 GiB，可用 `-cache-gib` 调整；包含已下载块、完整解压成员和正在写入的预留空间。LRU 淘汰未使用内容，打开的解压文件保持占用直到关闭。单个成员超出预算或所有可用空间被占用时返回空间不足。解压时还需要留出读取压缩块的余量。
- 缓存文件原子发布、私有权限，异常退出的临时文件下次启动清理；按云端版本和 HTTP 验证器区分内容。没有可用验证器时不跨读取器复用缓存。缓存是工作缓存而非离线副本，需要联网解析直链和验证源文件。

## 支持边界

首版支持普通文件、非加密的单卷 ZIP（Store / Deflate，包括标准库支持的 ZIP64）。加密 ZIP、其他压缩算法、7z/RAR、分卷 ZIP、嵌套压缩包自动映射暂不支持。ZIP 内部的另一个 ZIP 作为普通成员文件读取。

要求下载源返回正确的 HTTP 206 和 Content-Range；不支持 Range 时明确失败，不会悄悄下载整个大文件。读取期间实体验证器变化会报错，避免拼接不同版本。源站完全不提供验证器时无法保证读取期间源文件不变。Store 随机读取不做全成员 CRC 扫描；Deflate 完整解压会校验 CRC。

ZIP 名称当前要求有效 UTF-8，旧编码文件名会明确报错；拒绝路径穿越、符号链接、重名和文件/目录冲突。ZIP 最多 100,000 个条目，索引读取预算 64 MiB；不对成员文件设置额外的 32 MiB 限制。ZIP 索引保存在内存，磁盘缓存预算不包含索引内存、令牌文件及文件系统元数据开销。

## 验证

```bash
go test -race ./...
go vet ./...
# 真实 FUSE 挂载，使用本地模拟 API 数据，不访问个人网盘：
MOUNT123_FUSE_TEST=1 go test -race -run TestActualFUSEMount -v ./internal/mountfs
```

测试覆盖 API 授权/分页、缓存预算与并发、HTTP Range 验证、普通文件和 ZIP 随机读取，以及实际挂载后的写入/创建/删除/重命名拒绝。尚未用真实 123 账户完成端到端验收。
