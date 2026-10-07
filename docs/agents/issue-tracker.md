# Issue tracker: GitHub

仓库：binghezhouke/123。使用 gh CLI 管理 Issues。
发布任务时，每个任务创建一个 Issue，包含交付行为、验收标准和阻塞关系。
多行正文通过 --body-file 传入。
按依赖顺序创建，优先使用 GitHub 原生阻塞关系；不可用时在正文中引用阻塞 Issue。
已确认可执行的任务标记 ready-for-agent。
执行任务前确认其阻塞项均已完成；ready-for-agent 本身不表示没有阻塞。
PRs as a request surface: no.
