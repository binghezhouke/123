# 可重复的 Web/FUSE 真实验收（GitHub #63）

`live_acceptance.py` 使用 Python 3 标准库，对**已运行**的 mount123 Web 与 FUSE 挂载做只读抽样。默认不访问服务：

```sh
python3 mount123/tools/live_acceptance.py
python3 -m unittest discover -s mount123/tools/tests -v
```

第一条输出 `skipped/live_not_enabled` 并以 0 退出。第二条使用临时普通目录及本地 HTTP fixture，不需要网盘账号、FUSE、运行中的 mount123 或额外 Python 包。它不是 FUSE 内核集成测试。

具备 FUSE 和 Python 3 的 Linux 环境还可运行渐进目录的内核回归：

```sh
cd mount123
MOUNT123_FUSE_TEST=1 go test -race ./internal/mountfs \
  -run '^TestActualFUSELiveAcceptanceProgressiveRARConverges$' -count=1 -timeout=45s
```

该测试使用合成 RAR、本机 Range 服务和独立临时挂载，先让真实 CLI 扫描部分目录，再释放后续索引并重新打开目录验证完整集合。Web entries 使用本机协议 fixture，不是 webserve；测试不连接网盘或使用生产挂载。默认不设置环境变量时跳过。它覆盖部分目录收敛，`EAGAIN` 分支仍由离线边界测试覆盖。

## 准备样本

复制 `live_acceptance.example.json` 到仓库外的私有文件，替换其中的**虚构** ID、相对挂载路径、成员路径、精确文件大小及预期目录数量。不要将真实账号目录、凭证、密码、Cookie、Authorization、下载直链或签名 URL 放进仓库。`name` 是报告中唯一的样本身份，使用不包含隐私的 ASCII 别名。

每个 case 必须指定：

- `kind=archive`、顶层 `file_id`；或者 `kind=browse`、云目录 `parent_id`（0 表示挂载的云根目录）。
- `mount_path`：相对于 `--mount` 的路径，禁止绝对路径或 `..`；`browse` 可设为空字符串以比较挂载根目录。
- `directories`：按执行顺序列出的准确相对目录路径；每步带 `archive_depth`（顶层档案为 0，每进入一个档案加 1，普通文件夹不增加）。可用 `count` 检测两端同时少项。
- `order=outer-first` 的第一步必须为空路径，即先枚举外层；`direct` 从指定内层直接进入，不先列外层。
- 可选 `reads`：只列出明确允许读取的小样本，每项必须有精确 `size` 和 `archive_depth`。可配置 `offsets` 明确给出随机/反向读取顺序，规则见下文。`magic_hex` 验证头部格式标记；可选 `head_sha256`/`tail_sha256` 验证独立可信摘要。ZIP、7z、RAR、ISO 和分卷通过实际路径表示，脚本不会根据后缀猜测格式或自动发现档案。

`browse` 每个 case 只比较一个云目录；脚本跟随页面的分页 offset，不递归进入孩子。档案脚本跟随 JSON cursor，要求最终 `complete=true`、无重复成员和 cursor 循环、枚举数量等于最终 `total`，并比较 Web/FUSE 的完整名称集合。`complete` 表示索引完成，**不**代表当前页已覆盖全部目录。巨型固实 7z 可以只列已知浅层目录，不配置 `reads`，也不要猜测深层路径。

档案目录返回 HTTP 202 时，仅接受 `code=archive_index_pending`、`retryable=true`、空 `items`、`complete=false`、`total=0` 和未改变的当前 cursor。HTTP 200 的空 `items`、`complete=false` 则表示索引仍在增长，要求 `total` 为非负整数、`next_cursor` 为字符串；非空新 cursor 可推进扫描位置。两种状态均保留先前收到的行，等待后重试，不占 `--max-pages`，每个目录共享最多 8 次重试。每次 HTTP 仍计入操作和响应字节预算，等待也计入 `--max-seconds`。合法 `Retry-After` 秒数或 HTTP 日期决定等待时间，缺失时等待 1 秒；超过剩余全局时间时直接报预算失败，不提前重试，非法头报协议失败。带成员行的重复 cursor 仍报分页错误。400/403 等实际错误、坏 202 和预算耗尽均不会被等待逻辑掩盖。

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

`--case` 可重复；省略则执行 manifest 所有 case。重新运行上述命令加 `--resume` 会跳过已通过 case，重跑失败及未完成 case。输入 manifest 字节、目标和运行参数必须与原报告一致；含 `offsets` 的 manifest 还绑定显式区间读取契约版本，不能恢复旧脚本忽略 `offsets` 后得到的 head/tail 通过报告。该契约依据 manifest 全部 case，切换 `--case` 子集不改变身份；未含 `offsets` 的旧 manifest 身份保持兼容。更换部署或希望重验已通过 case 时使用新报告，**不要**用 resume。每个 case 完成后原子写入权限 0600 的报告，中途退出后已完成的 case 仍可恢复；一个正在执行但未完成的 case 会重新开始。

不启用 `--live` 时不读取 manifest、挂载或凭证。有 `--live` 时认证失败、服务不可用和超时均是失败，不会被标为通过或自动跳过。如果 Web 需要已有会话，可以通过私有环境提供 `MOUNT123_ACCEPTANCE_COOKIE` 或 `MOUNT123_ACCEPTANCE_AUTHORIZATION`；脚本不读取 mount123 配置、不申请 token、不处理密码。不要在命令行字面量或 shell history 中输入凭证。服务端已有的解锁状态会被使用。

localhost/loopback 请求单独禁用环境代理；内网 HTTP 和远程 HTTPS 均可配置，非 loopback 地址保留标准环境代理行为。不修改全局代理设置。HTTP 重定向一律拒绝，避免将会话头转发给 CDN 或登录地址，也不记录 Location。当前阶段检验的是 mount123 的代理成员 Range；**尚未**覆盖真实 CDN 重定向与直链过期恢复。

## READDIRPLUS、顺序与缓存解释

档案的首次 FUSE 目录句柄可能冻结尚未完成的目录，也可能暂时返回 `EAGAIN`。脚本仍先执行这一轮 FUSE 触发，再等待 Web 分页完整结束；不会先用 Web 暖索引。只有 `kind=archive` 的 FUSE `EAGAIN` 或相对完整 Web 名称集的严格子集才允许重新打开目录：每步最多追加 8 次 fresh `os.scandir`，每次前等待 0.1 秒，不复用旧目录句柄，也不重新请求 Web。扫描与等待共享原来的总时间和操作预算，不重置任何预算。固定次数内仍缺项报 `directory_mismatch`，仍为 `EAGAIN` 报 `budget/fuse_pending_budget`；全局预算先耗尽则保留对应预算错误。额外名称、重复名称、其他 errno、操作超时和无法回收的 worker 均立即失败，不用后续正确结果覆盖。`browse` 不使用这项渐进协调。

每个目录步在成功或失败时均保存 `first_fuse`、`fuse_attempts`、`fuse_attempt_count` 和 `fuse_wait_seconds`。每次尝试仅保存固定状态、数量、名称集合摘要、耗时及白名单错误字段，不保存原名称和路径。状态 `observed` 表示已取得快照但 Web 尚未核实完整结果；`pending` 表示 `EAGAIN`；Web 完整后才可判定 `partial`、`complete` 或 `mismatch`，重复项为 `duplicate`，其他扫描错误为 `failed`。因此 Web 失败时首轮可以保留 `observed` 或 `pending`，不能宣称目录完整。`fuse_attempts[].seconds` 是包含 worker 启动的墙钟尝试时间；成功步的 `fuse_seconds` 仍是最终成功 worker 的目录 I/O 操作时间，不包含 worker 启动。`fuse_wait_seconds` 是追加扫描的累计计划等待秒数。

首次 `partial` 或 `pending` 后通过只证明有界最终一致，不能证明首次扫描完整。FUSE 名称快照没有 `complete` 标记，客户端不能单靠严格子集区分正常目录增长和真实漏项；首轮证据必须保留。离线临时目录测试通过 HTTP 完成响应前新增文件验证 fresh scan 路径，模拟 `EAGAIN` 的测试不是内核 FUSE 证据；现有 Go 冻结目录句柄测试是独立契约证据。

每步先启动真实 `os.scandir`，并对枚举项执行 `entry.stat(follow_symlinks=False)`，然后请求 Web 分页。这样在进入下一层前已走过 FUSE 的目录枚举和 inode 属性路径，能够触发曾经的“先列外层，内层重复外层内容”问题。是否发送 READDIRPLUS 由挂载能力及内核决定，脚本不能凭 `os.scandir` 断言线级 opcode；需要精确确认时使用已有 FUSE 诊断设施。

复现 #90 应使用**已设置长正/负缓存 TTL** 的真实挂载。TTL=0 的 fixture 可能掩盖 inode 被目录枚举提前创建的问题。本工具不修改 TTL、不重挂载、不重启、不刷新、不探测、不清缓存、不删除索引。outer-first 与 direct 对照应选两个独立、尚未触碰的样本；先访问同一样本再做对照无法恢复冷状态。报告记录第一轮及重复轮耗时，不能将第一轮称为冷缓存测量。FUSE 先读、Web 后读，后者通常已经暖缓存；共享服务其他流量也会影响耗时与统计。

`--cache` 只注明有配置缓存位置，不遍历缓存目录、不读缓存文件、不写入该路径。`--control-socket` 可选，仅发送 `io-stats`。`stats_before`/`stats_after` 保留导出器固定白名单中的原始数值、固定枚举组状态及启动时间摘要 `epoch`，不抄写配置、路径、下载链接、绝对时间字符串或错误原文。`stats_delta` 仅包含已定义的累计计数器：源下载/前台读取/缓存命中/预读的事件与字节，缓存和恢复事件、API 与 FUSE 请求、完成的队列任务，以及时延样本数和累计纳秒。缓存驻留量、容量、活跃数、队列等待数、速率、p50/p95/max 时延保留在前后快照中，不计算差值；分位数不能解释为本 case 的独立时延。

`stats_delta_status` 说明这一窗口的比较结果：

启动时间须符合 RFC3339（支持 `Z` 和合法的 `±HH:MM` 偏移），日期与偏移均通过校验；报告只保存完整导出字符串的摘要，保留纳秒精度，不输出原始时间。同一运行期的导出格式应保持一致。

| 状态 | 含义 |
| --- | --- |
| `measured` | 两次取样成功、`epoch` 相同、运行时间未倒退，且已有可比较计数器；这些计数器的 `0` 表示该窗口无增量 |
| `unavailable/sample_unavailable` | 未配置统计、取样失败或没有完成取样；`stats_delta` 为空，不表示零下载 |
| `unavailable/no_comparable_counters` | 取样成功但没有双方均可解释的累计字段 |
| `not_comparable/epoch_unknown`、`process_changed` | 缺少有效启动时间标识，或服务运行期发生改变；即使新进程 uptime 更大也不跨进程求差 |
| `not_comparable/sample_time_unknown`、`sample_time_reversed` | 运行时间缺失、无效或倒退 |
| `not_comparable/counter_reset`、`invalid_counter` | 已知累计计数器下降，或值不符合非负整数定义；整个窗口的增量摘要为空，原始前后值仍保留 |

首次事件之前，导出器的七个事件/字节组及固定时延组会返回 `status=unknown`、数值为 null，脚本的数值快照因此省略这些字段。只有该已知组确实存在、符合这种无观测形状、且运行期可比较时，才将省略值视为累计零；例如第一次下载后仍报告 `downloaded_bytes.events=1` 和相应字节增量。整个组缺失、未知字段或 `measured` 组缺少字段不补零。`directory_lookup` 顶层组目前没有观测 hook，不推断其零值。`stages` 累计值须双方 `stage_schema_version=1`；缓存省略的取消/错误计数须双方 `fill_outcome_version=1`，旧版本缺失视为未知。`stats_delta` 中未出现的字段表示未定义或不可比较，与显式 `0` 不同。

旧报告若没有 `stats_delta_status`，其差值不能按上述规则解释；需要新的统计窗口时使用新报告。统计不可用不替代目录和字节一致性检验，共享服务其他流量也会计入这些进程累计增量。

## 数据验证与预算

每个成员读取最多 64 KiB 的 head 和 tail（小于 64 KiB 的文件只读其实际长度，两端可能重叠），默认重复两轮；可用 `--range-edge head --repeats 1` 仅做一次头部比较，避免尾部随机读。每次必须得到 206、准确 Content-Range、identity 编码、完整长度，且 Web 与 FUSE **逐字节相等**；重复读摘要必须相等。报告保存 SHA256、16 字节签名及两端操作耗时（不含 Python 子进程启动；墙钟 deadline 仍包含启动成本）。没有独立 magic/摘要时，只能证明两条路径结果相同，不能证明原始格式正确。HTTP 200、错误 HTML、超时、短读均不能判成功。

每个 `reads` 样本可选 `offsets`：非空整数数组，最多 32 项，每项须满足 `0 <= offset < size`；布尔值、负数、浮点数和越界值均为配置错误。允许乱序及重复，例如 `[12582912, 4194304, 10485760, 2097152, 12582912, 0]`。配置后完全替代该样本的默认 head/tail，在每轮 `--repeats` 中严格按数组顺序读取，每次长度为 `min(65536, size-offset)`，近 EOF 不越过文件结尾。不自动生成偏移、不发现新文件、不增加独立预算；每个偏移的 FUSE 与 HTTP 操作仍计入原操作、响应字节和总时间预算。

所选 case 含 `offsets` 时不能同时使用 `--range-edge`。`offsets` 样本也不能带 `head_sha256` 或 `tail_sha256` 字段；这些独立摘要仍只用于默认 head/tail 模式，禁止静默忽略或拿它们校验不同长度的显式区间。显式 offset 0 仍检查 `magic_hex`；没有请求 offset 0 时不检查文件头格式标记。同一 `(offset,length)` 的摘要在数组内重复位置和跨轮次均须一致，即使两端同时发生相同变化也会失败。

显式模式的每条成功 Range 只报告数值 `offset`、从 1 开始贯穿本样本全部轮次的 `sequence`、`repeat`、`bytes`、SHA256 和两端耗时，不保存中间内容的十六进制签名、名称或路径。默认模式保留原 `edge` 与头尾签名字段。每个样本先挂接空的 `ranges` 列表，再依次追加已通过所有校验的区间；失败时保留之前完成的证据，失败区间不记为已通过，整个 case 仍失败。后续统计的普通预算失败只记录统计不可用，不覆盖先前的协议或完整性错误；无法回收 worker 仍必须终止整套流程。

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

`--timeout` 和 `--max-seconds` 必须为有限正数。成功目录页可额外记录 `pending_responses` 和 `pending_wait_seconds`，分别表示该页的等待响应数及按服务端要求等待的秒数；不保存 `Retry-After` 原文、pending 正文或 cursor。

## 结果解释与尚未覆盖项

退出码 0 表示所选 case 全通过，或未启用 live 的正常跳过；1 表示验收失败；2 表示配置或启动失败。报告不含路径、ID、cursor、请求 URL、响应正文、响应头原文或异常文本，仅保留别名、计数、固定错误码、摘要、耗时及白名单统计。

错误分类表示观测证据，不自动推断根因：`authentication` 对应 401/403 或登录跳转；`platform` 对应可见的 429；`format_or_policy` 对应格式签名或可辨识的内核策略错误；`pagination`、`protocol`、`integrity`、`mount`、`web`、`timeout`、`budget`、`environment` 按失败位置区分。Web 将底层错误折叠为 400 时只能记录 `web/http_status`，不能声称已经区分格式、平台与挂载器根因。服务日志需在本机另行诊断，禁止原样拷贝带凭证内容。

#77 已增加三层归档组合回归；真实验收仍须在 manifest 中准确声明深度并设置对应的 `--max-depth`，未验证的组合不能视为已通过。此工具是 #63 的最小可重复 smoke 流程；平台 API 429 注入/恢复、401/403 后刷新、直链过期、CDN 重定向、TTL 自然失效、加密侧车变化、自动重挂载后缓存复用还没有自动化真实验证，不能据此关闭整个 #63。手动重启前后的同一组样本报告可以比较缓存复用，但脚本不会替用户重启服务。
