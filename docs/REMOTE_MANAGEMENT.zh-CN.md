# 远程管理说明

## 环境要求

GUI 控制端可以是 Windows、macOS 或 Linux。执行机目前支持 Linux/macOS，需要 SSH 服务、普通用户可写的 workspace，以及到来源/目标 Registry 的网络。执行机不需要 Docker；`skopeo` 也是可选项。

来源镜像必须能通过已保存的 Registry V2 配置访问。工具刻意不读取执行机本地 Docker daemon 的镜像，从而保证没有安装 Docker 时也具有一致行为。

Harbor 管理支持两种明确路径：“本机直接访问”由控制端 `dia` 进程请求 Harbor；“经执行机”通过 SSH 在远端请求。Registry 同步和 `local_tar` 仍然必须选择执行机。

## 配置流程

1. 在“连接设置”保存来源和目标 Registry/Harbor；Endpoint 使用 `https://host[:port]`，namespace 可选。
2. 为每个站点保存一个或多个普通账号/robot account。
3. 保存 SSH 执行机，选择 SSH agent、私钥或密码认证。
4. 点击“测试”，独立核对首次显示的 SHA-256 host-key 指纹后确认。
5. 在“镜像同步”选择执行机；需要长期使用时点击“设为默认执行机”。
6. 新建并保存镜像清单，选择来源、目标、账号和传输引擎后启动。

Harbor 页面单独提供“访问路径”选择。配置中的代理由实际执行请求的控制端或远端 agent 使用，密码始终只在后端解密。

首次读取 host key 时不会发送保存的 SSH 密码。以后指纹变化会直接拒绝连接，不会自动覆盖。

## 镜像清单

```text
# 注释
team/api:v1
team/worker:v1 -> archive/worker:v1
```

普通行保留相同目标名；`->` 可以重命名。来源和目标分别应用各自 profile 的 namespace。`local_tar` 中映射到同一输出文件的重复项会在下载前报错。

对于 `local_tar`，右侧目标同时决定远端文件名和 tar 内的 `RepoTags`。单平台包使用该目标名；多平台导出会给每个文件名和 tag 追加 `-<os>-<arch>[-<variant>]`。来源可以使用 digest；目标必须使用 tag，因为 docker-load 的 `RepoTags` 无法表示 digest。

## Registry 目标

原生引擎会递归复制 manifest list、子 manifest、config 和 layer：

- 目标已有 blob 时跳过
- 同 Registry 不同仓库时先尝试 cross-repository mount
- mount 不成功才从来源流式读取并推送目标
- manifest 和 blob 保留并验证上游声明的 digest 算法

`auto` 在执行机已安装可选的 [`skopeo`](https://github.com/containers/skopeo) 且代理、账号兼容时优先使用；某项失败会自动用原生引擎重试。强制 `skopeo` 模式不会回退。

## `local_tar` 目标

不指定输出根时，执行机会探测配置的 storage roots、workspace、用户 archive 目录和可写挂载点，选择可用空间最大的候选文件系统。每个平台单独生成 tar，并在原子替换前完成 digest、diffID 和 docker-load 结构校验。

完成后任务面板显示：

- 每个平台 tar 绝对路径和大小
- `*_platforms.json`
- 对应的 `docker load -i` 命令

右侧“最近任务”可以在当前进程保留的任务之间切换。刷新浏览器后会自动重新接管最新的运行中任务；完成、失败或取消的任务保留 10 分钟。该历史仅存于内存，不会把任务参数或凭据持久化到磁盘。

## 远程文件

浏览目录只返回名称、大小、时间等元数据。点击“下载”或把文件拖出浏览器时会先确认；取消后不会读取文件内容。

远端只能访问 workspace、storage roots 和自动选择的 archive 目录。符号链接无法越出这些边界；删除链接只删除链接本身。回传前远端计算完整 SHA-256，程序化下载使用 `.part`、完整校验和原子替换。

## 常见问题

- **对端没有 Docker：** 不影响，所有操作都不调用 Docker。
- **对端没有 skopeo：** `auto`/`native` 会使用内置 Registry V2。
- **开发版无法自动部署不同架构：** 使用正式 release 控制端，或先在执行机安装对应 release。
- **auto 没选 skopeo：** 来源/目标代理不同，或同一 Registry 主机用了两个不同账号时会主动选择原生引擎。
- **磁盘选择不符合策略：** 查看执行机磁盘结果，配置 storage roots，或填写允许范围内的明确输出根。
