# docker-image-saver (`dia`)

[English](README.md)

`dia` 是一个不依赖 Docker daemon 的 Docker/OCI 镜像导出、Registry 同步和 Harbor 管理工具。发布产物是单个静态可执行文件，同时提供 CLI、无参数终端向导和本地 Web GUI。

## 主要能力

- 直接实现 Registry HTTP API V2，不调用 Docker、Podman 或 containerd。
- 原生引擎没有外部运行时依赖；对端没有 Docker、没有 `skopeo` 也能工作。
- layer 直接流入最终 docker-load tar，不落中间 layer 文件。
- 多架构镜像按 `os/arch[/variant]` 分别保存，并生成 `*_platforms.json`。
- 默认校验 manifest、config、blob、解压后的 `diff_id` 和最终 tar 目录结构。
- Linux、macOS、Windows 和 Termux 均有 CI 构建产物。
- Registry 同步和远端 `local_tar` 在用户选择的 Linux/macOS SSH 执行机上运行，镜像数据不经过 GUI 所在电脑。

## 启动方式

无参数进入终端 wizard：

```bash
dia
```

CLI：

```bash
dia --image alpine:latest --arch 1 --output alpine.tar
dia pull alpine:latest alpine.tar --arch 1
dia save nginx:latest --proxy socks5h://127.0.0.1:1080
```

GUI：

```bash
dia gui
dia --gui
dia gui --no-browser
```

GUI 只监听随机的 `127.0.0.1` 端口。无参数行为不会改成 GUI，仍然是终端向导。

## 远程镜像同步

“镜像同步”中的每个任务必须选择一台 SSH 执行机。GUI 通过 SSH stdio 启动一次性的 `dia remote-agent`，代理不监听远端 TCP 端口。

- Registry -> Registry：源 blob 流经执行机直接推送目标 Registry。
- Registry -> `local_tar`：执行机选择可写候选中可用空间最大的文件系统，保存经过完整校验的分平台 tar。
- GUI 本机只发送控制请求和凭据；只有用户在“远程文件”页确认下载或拖回时，文件才通过 SSH 传回。
- 远端机器不需要 Docker。
- 发布版可以校验 GitHub Release 的 SHA-256 后自动部署匹配系统/架构的远程代理。

同步来源统一使用 Registry V2 镜像引用；`dia` 不读取执行机本地 Docker daemon 中的镜像，因此在未安装 Docker 的机器上行为完全一致。

传输引擎：

[`skopeo`](https://github.com/containers/skopeo) 只是可选加速路径，不是运行依赖。

| 模式 | 行为 |
|---|---|
| `auto` | 执行机有 `skopeo` 且代理/账号兼容时优先使用；失败会自动回退内置 Registry V2。 |
| `native` | 始终使用内置实现，不执行外部命令。 |
| `skopeo` | 强制使用执行机上的 `skopeo`，失败直接报错，不隐式回退。 |

`local_tar` 始终使用原生校验引擎。

## 持久化镜像清单

GUI 提供可编辑并持久化的列表编辑框：

```text
# 注释和空行会忽略
team/api:v1
team/worker:v1
legacy/service:v2 -> archive/service:v2
```

没有 `->` 时来源和目标使用相同仓库/tag；有 `->` 时可重命名目标。来源和目标分别应用所选 Registry 配置中的 namespace。

目标为 `local_tar` 时，右侧名称同时决定远端文件名和 tar 内的 `RepoTags`。单平台包按该目标名加载；多平台导出会给每个 tar 文件名和 tag 追加 `-<os>-<arch>[-<variant>]`，避免平台互相覆盖。来源可以使用 digest 引用；`local_tar` 目标必须使用 tag，因为 docker-load 的 `RepoTags` 无法表示 digest。

## Registry、Harbor 和多个账号

“连接设置”可以保存多个 Registry/Harbor 站点，并为每个站点保存多个用户或 robot account。Harbor 页面支持：

- 健康状态
- 项目查询、创建、删除
- 仓库查询、删除
- artifact 查询、删除
- tag 创建、删除

Harbor 页面可以选择“本机直接访问”，由当前 `dia` 进程调用 Harbor；也可以选择保存的 SSH 执行机，让相同请求在远端执行。配置中的代理会在所选执行路径上生效，两种模式都不会把密码交给浏览器。所有删除操作都有二次确认。

## SSH 执行机

每台执行机可持久化：

- 名称、主机、端口、用户
- SSH agent、私钥或密码认证方式
- 私钥路径、远端 workspace、远端 dia 路径
- 候选存储根目录
- 已确认的 SHA-256 host-key 指纹

第一次测试连接时必须核对并确认 host key。可以临时选择执行机，也可以随时设置/更改默认执行机。

## 代理

支持 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 和 `NO_PROXY`（大小写变量均可），也支持显式代理：

```text
http://127.0.0.1:7890
socks5://127.0.0.1:7897
socks5h://127.0.0.1:7897
```

`socks5://` 使用本地 DNS，`socks5h://` 由代理解析域名。
环境代理会遵守 `NO_PROXY`；显式代理是当前请求的主动覆盖。

## 导出与手动加载

成功后 GUI/CLI 会显示绝对路径、大小、平台索引和手动加载命令：

```bash
docker load -i /absolute/path/image_linux_amd64.tar
docker image ls
```

每个平台 tar 都包含 `manifest.json`、`repositories`、config JSON 和 layer 目录。工具先写 `.part` 临时文件，完成哈希与结构校验后再原子替换正式文件。

## 配置与密码

默认保存在系统用户配置目录下的 `dia/`：

- `config.json`：站点、用户名、SSH 元数据、镜像清单，不含密码
- `secrets.enc`：AES-256-GCM 加密的 Registry/SSH secret
- `master.key`：随机本机密钥，在支持的平台上权限为 `0600`

可选环境变量：

- `DIA_CONFIG_DIR`：修改配置目录
- `DIA_CONFIG_KEY`：外部提供的 32 字节 base64 密钥，应在第一次保存 secret 前设置
- `DIA_REGISTRY_USERNAME` / `DIA_REGISTRY_PASSWORD`：临时 CLI/GUI 启动凭据

浏览器只会拿到 `has_secret`，不会拿到保存的密码。密码不会进入 HTML bootstrap、任务快照、命令行或日志。
终端向导在真实 TTY 中读取密码时关闭回显；通过管道输入时仍可用于自动化。

更完整的操作流程见 [远程管理说明](docs/REMOTE_MANAGEMENT.zh-CN.md)，安全边界见 [安全模型](docs/SECURITY.zh-CN.md)。

## 安装和 Termux

发布文件命名为 `dia_<os>_<arch>`。Termux 直接使用标准 Linux 二进制，例如：

```bash
chmod +x dia_linux_arm64
./dia_linux_arm64
```

不需要 root，也不需要 APK。

## 开发门禁

本地只允许代码检查以及语法、格式校验。单元/集成测试、竞态检测、`go vet`、交叉编译、打包、校验和生成与 Release 发布必须全部在 GitHub Actions 中完成。所有变更必须通过 Pull Request 进入 `main`，且 `quality` 与 `cross-build` 两项必需检查均通过后才能合并。

完整流程见 [CONTRIBUTING.md](CONTRIBUTING.md)，强制仓库策略见 [AGENTS.md](AGENTS.md)。

## CI 与发布

分支 push 和 Pull Request 会运行 [.github/workflows/ci.yml](.github/workflows/ci.yml)。推送已在 `CHANGELOG.md` 中登记的 `v*` tag 后，[.github/workflows/release.yml](.github/workflows/release.yml) 将在 GitHub runner 上依次完成：

1. 格式、GUI 语法和 Go module 可复现性检查
2. `go vet`、竞态测试和乱序测试
3. 全部有效 Linux、macOS、Windows 目标交叉编译
4. SHA-256 校验和生成
5. GitHub Release 说明与资产发布

禁止把本地构建的二进制上传为 Release 资产。
