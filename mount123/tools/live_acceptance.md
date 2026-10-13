# 可重复的 Web/FUSE 真实验收（GitHub #63）

`live_acceptance.py` 使用 Python 3 标准库，对**已运行**的 mount123 Web 与 FUSE 挂载做只读抽样。默认不访问服务：

```sh
python3 mount123/tools/live_acceptance.py
python3 -m unittest discover -s mount123/tools/tests -v
```

第一条输出 `skipped/live_not_enabled` 并以 0 退出。第二条使用临时普通目录及本地 HTTP fixture，不需要网盘账号、FUSE、运行中的 mount123 或额外 Python 包。它不是 FUSE 内核集成测试。

## 准备样本

复制 `live_acceptance.example.json` 到仓库外的私有文件，替换其中的**虚构** ID、相对挂载路径、成员路径、精确文件大小及预期目录数量。不要将真实账号目录、凭证、密码、Cookie、Authorization、下载直链或签名 URL 放进仓库。`name` 是报告中唯一的样本身份，使用不包含隐私的 ASCII 别名。

每个 case 必须指定：

- `kind=archive`、顶层 `file_id`；或者 `kind=browse`、云目录 `parent_id`（0 表示挂载的云根目录）。
- `mount_path`：相对于 `--mount` 的路径，禁止绝对路径或 `..`；`browse` 可设为空字符串以比较挂载根目录。
- `directories`：按执行顺序列出的准确相对目录路径；每步带 `archive_depth`（顶层档案为 0，每进入一个档案加 1，普通文件夹不增加）。可用 `count` 检测两端同时少项。
- `order=outer-first` 的第一步必须为空路径，即先枚举外层；`direct` 从指定内层直接进入，不先列外层。
- 可选 `reads`：只列出明确允许读取的小样本，每项必须有精确 `size` 和 `archive_depth`。`magic_hex` 验证头部格式标记；可选 `head_sha256`/`tail_sha256` 验证独立可信摘要。ZIP、7z、RAR、ISO 和分卷通过实际路径表示，脚本不会根据后缀猜测格式或自动发现档案。

`browse` 每个 case 只比较一个云目录；脚本跟随页面的分页 offset，不递归进入孩子。档案脚本跟随 JSON cursor，要求最终 `complete=true`、无重复成员和 cursor 循环、枚举数量等于最终 `total`，并比较 Web/FUSE 的完整名称集合。`complete` 表示索引完成，**不**代表当前页已覆盖全部目录。巨型固实 7z 可以只列已知浅层目录，不配置 `reads`，也不要猜测深层路径。

## 运行及恢复

```sh
python3 mount123/tools/live_acceptance.py --live \
  --manifest /private/acceptance-samples.json \
  --base-url http://127.0.0.1:8082 \
  --mount /path/to/mount \
  --cache /path/to/cache \
  --control-socket /private/mount123-control.sock \
  --case split-outer-first \
  --report /private/acceptance-report.json
```

`--case` 可重复；省略则执行 manifest 所有 case。重新运行上述命令加 `--resume` 会跳过已通过 case，重跑失败及未完成 case。输入 manifest 字节、目标和运行参数必须与原报告一致；更换部署或希望重验已通过 case 时使用新报告，**不要**用 resume。每个 case 完成后原子写入权限 0600 的报告，中途退出后已完成的 case 仍可恢复；一个正在执行但未完成的 case 会重新开始。

不启用 `--live` 时不读取 manifest、挂载或凭证。有 `--live` 时认证失败、服务不可用和超时均是失败，不会被标为通过或自动跳过。如果 Web 需要已有会话，可以通过私有环境提供 `MOUNT123_ACCEPTANCE_COOKIE` 或 `MOUNT123_ACCEPTANCE_AUTHORIZATION`；脚本不读取 mount123 配置、不申请 token、不处理密码。不要在命令行字面量或 shell history 中输入凭证。服务端已有的解锁状态会被使用。

localhost/loopback 请求单独禁用环境代理；内网 HTTP 和远程 HTTPS 均可配置，非 loopback 地址保留标准环境代理行为。不修改全局代理设置。HTTP 重定向一律拒绝，避免将会话头转发给 CDN 或登录地址，也不记录 Location。当前阶段检验的是 mount123 的代理成员 Range；**尚未**覆盖真实 CDN 重定向与直链过期恢复。

## READDIRPLUS、顺序与缓存解释

每步先启动真实 `os.scandir`，并对枚举项执行 `entry.stat(follow_symlinks=False)`，然后请求 Web 分页。这样在进入下一层前已走过 FUSE 的目录枚举和 inode 属性路径，能够触发曾经的“先列外层，内层重复外层内容”问题。是否发送 READDIRPLUS 由挂载能力及内核决定，脚本不能凭 `os.scandir` 断言线级 opcode；需要精确确认时使用已有 FUSE 诊断设施。

复现 #90 应使用**已设置长正/负缓存 TTL** 的真实挂载。TTL=0 的 fixture 可能掩盖 inode 被目录枚举提前创建的问题。本工具不修改 TTL、不重挂载、不重启、不刷新、不探测、不清缓存、不删除索引。outer-first 与 direct 对照应选两个独立、尚未触碰的样本；先访问同一样本再做对照无法恢复冷状态。报告记录第一轮及重复轮耗时，不能将第一轮称为冷缓存测量。FUSE 先读、Web 后读，后者通常已经暖缓存；共享服务其他流量也会影响耗时与统计。

`--cache` 只注明有配置缓存位置，不遍历缓存目录、不读缓存文件、不写入该路径。`--control-socket` 可选，仅发送 `io-stats`。报告包含每个 case 前后的数值型统计以及算术差值，不抄写配置、路径或错误原文。计数器重置会产生负差；延迟分位数/活跃数等是 gauge，算术差不能解释为此 case 的独立延迟或请求数。未配置或暂不可用时注明 `available=false`，不会伪造缓存命中证据。

## 数据验证与预算

每个成员读取最多 64 KiB 的 head 和 tail（小于 64 KiB 的文件只读其实际长度，两端可能重叠），默认重复两轮；可用 `--range-edge head --repeats 1` 仅做一次头部比较，避免尾部随机读。每次必须得到 206、准确 Content-Range、identity 编码、完整长度，且 Web 与 FUSE **逐字节相等**；重复读摘要必须相等。报告保存 SHA256、16 字节签名及两端操作耗时（不含 Python 子进程启动；墙钟 deadline 仍包含启动成本）。没有独立 magic/摘要时，只能证明两条路径结果相同，不能证明原始格式正确。HTTP 200、错误 HTML、超时、短读均不能判成功。

默认串行执行，并发固定为 1。预算参数：

| 参数 | 默认 | 限制 |
| --- | --- | --- |
| `--timeout` | 40 秒 | 每个独立 HTTP/FUSE/控制操作的墙钟期限 |
| `--max-seconds` | 180 秒 | 本次运行总墙钟预算 |
| `--max-bytes` | 16 MiB | HTTP 正文和 FUSE 读取返回给脚本的字节总量 |
| `--max-page-bytes` | 1 MiB | 单个目录页正文 |
| `--max-pages` | 32 | 每个目录的 Web 分页数 |
| `--max-entries` | 2000 | 每个目录枚举项数 |
| `--max-operations` | 100 | 本次运行子进程操作数，含统计 |
| `--max-depth` | 2 | manifest 声明的嵌套档案深度 |
| `--repeats` | 2 | 每个样本 head/tail 比较轮数 |

深度来自人工 manifest 声明，不能替代服务自身的递归策略。响应字节预算**不等于网盘下载预算**：档案索引、固实解压、后台预取可能在服务内读取远多于 64 KiB，终止客户端也不能保证服务立即停止后台工作。选择事先已知代价的小样本；不要在未评估的 GB 级固实包上配置成员读取。脚本不自动全盘发现、不递归树扫描、不批量下载。缓存统计可供事后评估实际下载量，无法提供源端硬限额。

所有可能阻塞的操作在独立进程组中运行。超时杀死进程组并回收子进程；如果 Linux FUSE I/O 处于不可中断状态，SIGKILL 也可能暂时无法回收。此时记录 `worker_unreaped` 和 PID，立即停止整套验收，不再累积阻塞子进程。脚本不为了清理这种内核状态而重启或卸载服务。

## 结果解释与尚未覆盖项

退出码 0 表示所选 case 全通过，或未启用 live 的正常跳过；1 表示验收失败；2 表示配置或启动失败。报告不含路径、ID、cursor、请求 URL、响应正文、响应头原文或异常文本，仅保留别名、计数、固定错误码、摘要、耗时及白名单统计。

错误分类表示观测证据，不自动推断根因：`authentication` 对应 401/403 或登录跳转；`platform` 对应可见的 429；`format_or_policy` 对应格式签名或可辨识的内核策略错误；`pagination`、`protocol`、`integrity`、`mount`、`web`、`timeout`、`budget`、`environment` 按失败位置区分。Web 将底层错误折叠为 400 时只能记录 `web/http_status`，不能声称已经区分格式、平台与挂载器根因。服务日志需在本机另行诊断，禁止原样拷贝带凭证内容。

第三层档案在 #77 完成前仍可能失败，应保持真实失败记录，不配置为预期成功、不吞掉错误。此工具是 #63 的最小可重复 smoke 流程；平台 API 429 注入/恢复、401/403 后刷新、直链过期、CDN 重定向、TTL 自然失效、加密侧车变化、重挂载缓存复用还没有自动化真实验证，不能据此关闭整个 #63。
