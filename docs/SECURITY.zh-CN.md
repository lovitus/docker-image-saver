# 安全模型

## 本地 GUI

GUI 只监听随机的 `127.0.0.1` 端口，不提供公网监听选项。启动时生成 256-bit 随机 token，浏览器用它换取 HttpOnly、SameSite=Strict cookie 后跳转到不含 token 的 URL。API 同时校验 loopback Host、完全一致的 Origin 主机和端口，以及 constant-time cookie。

响应设置 CSP、`no-referrer`、`no-store`、禁止 frame 和 MIME sniffing。

“最近任务”API 使用相同的会话边界，只返回内存中的脱敏任务快照；终态快照在 10 分钟后删除，且从不持久化。

## 密码

Registry 密码、token、SSH 密码和私钥口令使用 AES-256-GCM 保存到独立 `secrets.enc`。浏览器只得到 `has_secret` 标志。密码不会写入 HTML bootstrap、设置 API、任务/SSE、平台索引、skopeo 命令行或日志。

默认 `master.key` 与 vault 位于同一用户配置目录，主要避免明文泄露和只备份 `config.json` 时泄密；它不能抵抗已经完全控制同一系统账号的攻击者。更强隔离可以在第一次保存 secret 前通过安全的外部环境提供 `DIA_CONFIG_KEY`。

临时认证代理 URL 的 userinfo 不会进入 HTML bootstrap、任务快照或错误文本；启动参数中的代理凭据保留在后端，并由单独复选框启用。Registry 配置禁止保存带 userinfo 的代理 URL，长期认证代理应配置在执行机环境中。

## SSH

- 首次读取 host key 时，回调会在发送密码认证前终止连接。
- 保存的 SHA-256 指纹严格匹配，变化时 fail closed。
- 远端代理只通过 SSH stdio 处理单次请求，不监听端口。
- SSH stdin 关闭会取消远端 Registry 请求和 skopeo 子进程。
- 自动部署 release 代理时先校验 SHA-256，安装后再验证版本。

## 远端文件和并发

远端文件路径必须位于 canonical workspace/storage/archive roots 下。父级符号链接会先解析；下载只允许普通文件；不支持删除目录，也不能删除配置根目录。

正式 tar 只会被完整校验后的同目录临时文件原子替换。GUI 会预留所有派生 tar 和平台索引的 canonical 路径，避免并发任务同时截断同一输出。

## Registry 完整性

内置引擎校验 manifest、config、压缩 blob、descriptor size、解压 layer diffID 和最终 docker-load tar。支持已实现的 `sha256`、`sha384`、`sha512`，不支持的 digest 算法会直接报错。

TLS 证书校验默认开启；`Insecure TLS` 只应在受控测试环境使用。
