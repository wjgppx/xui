# 发布、低中断更新与失败回退

`scripts/release.py` 管理本机已构建、已审查的发布目录。它以原子符号链接切换控制面程序，验证 `/readyz` 中的**发布标识和数据库版本**，失败时恢复上一版本并再次验证。它不下载或解压归档，不接收在线上传，不修改或降级数据库。

## 构建与打包

先执行完整测试、备份与恢复演练，确认目标版本和上一个版本都支持即将使用的数据库结构及安全策略。发布标识只允许字母、数字、点、下划线和连字符，最长 64 字符。

```bash
release_id=0.16.0-example
mkdir -p .local/release-build
bash scripts/go.sh build -trimpath -ldflags "-X main.releaseID=$release_id" -o .local/release-build/xui-control ./cmd/control
bash scripts/go.sh build -trimpath -o .local/release-build/xui-agent ./cmd/agent
npm --prefix web ci
npm --prefix web run build
python3 -B scripts/release.py pack --root /srv/xui --release "$release_id" \
  --control .local/release-build/xui-control --agent .local/release-build/xui-agent --web web/dist \
  --compatibility-epoch staff-mfa-v1 --schema-version 16 --schema-min 16 --schema-max 16
python3 -B scripts/release.py verify --root /srv/xui --release "$release_id"
```

这些命令应由发布目录的属主执行，构建产物须对该用户可读。首次创建 `/srv/xui` 时由主机管理员建立属主为 `xui`、权限 `0700` 的目录，再使用该身份执行工具。不要用 `chmod -R` 开放整个发布目录，也不要把密钥、环境文件、数据库文件放进发布树。`pack` 只接受两个指定程序及 `web/` 文件树；必须包含 `web/index.html`。

每次打包生成新的 `releases/<release_id>`，已有标识不能覆盖。文件清单记录 SHA-256、长度和执行位；目录权限 `0700`，普通文件与清单 `0600`，程序 `0700`。符号链接、硬链接、隐藏资源、路径穿越、过深路径、未列入清单的文件及超限文件会被拒绝。上限为 4096 个文件、单文件 128 MiB、总量 512 MiB。该校验用于检测本机产物变化，**不能代替来源签名、构建来源审查或依赖审计**。

## 启动配置与进程管理

模板位于 [xui-control.service](../deploy/systemd/xui-control.service) 和 [xui-agent.service](../deploy/systemd/xui-agent.service)。按实际主机修改路径、域名和账号后安装。模板不自动创建系统账号、目录、证书或环境文件，也不授予低于 1024 的端口权限。

控制面使用独立的 `xui` 系统账号、`/var/lib/xui` 工作目录和 `/etc/xui/control.env`。环境文件及 MFA 密钥文件权限设为 `0600`、属主为服务账号。生产配置包括 `APP_MODE=production`、HTTPS `PUBLIC_ORIGIN`、数据库和邮件配置、`MFA_KEY_FILE`、可信代理网段；详见安全与邮件验收文档。API 默认仅监听 `127.0.0.1:8080`，由 HTTPS 反向代理接入。`/readyz` 应由主机或管理网络访问。

服务在 SIGTERM 后等待在途 HTTP 和持久工作退出，systemd 最长等待 30 秒。重启期间新请求仍可能短暂失败；同机单实例切换不等于无中断滚动发布。负载均衡、多实例调度和迁移兼容性必须在实际拓扑上另行验收。重启限速可避免配置错误导致无限快速重启。

节点模板使用独立账号与 `/var/lib/xui-agent`，身份、SQLite outbox、WAL、证书目录须按备份文档管理。`KillMode=mixed` 先通知 Agent，使其采样、停 core 和刷新待投递流量；超时后才清理后代进程。替换示例控制面域名，并预先安装经校验的 Xray。不要随意改变已有节点的身份文件或删除待投递 outbox。

发布工具与服务目录属主相同，但 systemd 将发布树挂为只读。需要受限的主机重启入口时，可由主机管理员安装一个**固定路径、root 属主且普通用户不可写**的辅助程序，例如 `/usr/local/sbin/xui-restart-control`：

```sh
#!/bin/sh
set -eu
if [ -L /srv/xui/current ]; then
  exec /usr/bin/systemctl restart xui-control.service
fi
exec /usr/bin/systemctl stop xui-control.service
```

如使用 sudoers，只允许 `xui` 免密码运行这一条无参数辅助程序；不要授权通配命令、任意服务名或任意 shell。首次发布失败时没有上一版本，辅助程序中的 stop 分支用于停掉失败实例。

## 切换与回退

确认备份可恢复、环境配置和独立密钥已备份后，执行：

```bash
python3 -B scripts/release.py activate --root /srv/xui --release "$release_id" \
  --restart-json '["/usr/bin/sudo","-n","/usr/local/sbin/xui-restart-control"]' \
  --ready-url http://127.0.0.1:8080/readyz --timeout 20
```

重启命令是 JSON 参数数组，不经过 shell，参数中不要放密码或令牌。工具不输出命令参数和子进程输出。`--ready-url` 必须直接指向 `/readyz`；明文 HTTP 仅接受字面量回环 IP，其他地址必须 HTTPS。不会使用环境中的 HTTP 代理或跟随重定向。

切换前要求上一版本健康、身份匹配且两个版本的兼容范围覆盖预期数据库版本。每个发布清单必须明确 `schema_version`（该程序应迁移到的版本）、`schema_min` 和 `schema_max`（程序已验证支持的范围）。只有经过测试的兼容范围才能填写，不能为了跳过拒绝而扩大范围。候选版本就绪时数据库版本不得低于其迁移目标，也不能超出声明范围。旧服务继续返回 200、错误发布标识或旧数据库版本不会被当成新版本成功。

当前安全兼容代际为 `staff-mfa-v1`。工具拒绝跨代际切换，禁止自动回退到没有管理员二次验证的旧程序。升级到此代际的**首次引入**需要人工维护窗口、已验证备份以及独立配置；旧 0.15 程序不应作为此工具的自动回退候选。安全代际相同也不代表数据库变更一定可逆。

切换失败时工具恢复上一个 `current` 链接、重启并核验旧版本身份。若仍可探测到的数据库版本已超出旧程序支持范围，它会停止自动回退并保留恢复记录，避免启动不兼容旧程序。数据库不可探测时只能依赖切换前持久记录的兼容契约；因此不能在此流程中发布未声明、破坏性或未验证的迁移。

工具不会执行数据库 downgrade。不可逆迁移应采用多次兼容发布，或安排维护窗口及经过演练的数据库恢复；不能只回退二进制。数据库恢复还需评估备份后的订单、支付、会话和流量记录，不可直接覆盖生产库。

## 中断恢复

激活过程使用排他锁及 fsync 后的 `pending.json`。在进程被杀、重启失败或回退失败后，后续激活会被阻止。先检查服务、数据库与备份状态，不要手动删除恢复记录。具备相同环境和受限重启入口后执行：

```bash
python3 -B scripts/release.py recover --root /srv/xui \
  --restart-json '["/usr/bin/sudo","-n","/usr/local/sbin/xui-restart-control"]' \
  --ready-url http://127.0.0.1:8080/readyz --timeout 20
```

恢复会校验上一版本产物和兼容性，恢复链接、重启并确认身份；候选文件损坏也不阻止校验完整的旧版本。上一版本损坏、数据库不兼容或旧服务仍不可用时，记录保留并返回明确错误，由主机管理员处理。

`pack` 包含 Agent 和前端文件便于统一产物核验；`activate/recover` 的自动就绪核验针对控制面 API。Agent 必须按节点分批发布，并验证身份、TLS、租约、连接与 outbox 回传；二进制替换会重启 core，现有连接可能中断。新增用户的可选低中断更新见 [runtime-updates.md](runtime-updates.md)。前端静态文件应由反向代理另行发布，校验后复制到专用公开静态目录并切换其链接，保留旧带哈希资源以兼容缓存页面；不要把私有 `/srv/xui` 整棵暴露给 Web 服务器。

## 验证范围

```bash
python3 -B scripts/test_release.py
```

测试使用临时私有目录、真实子进程及回环 HTTP 服务，不重启本机业务服务。覆盖打包完整性、权限、路径与文件限制、成功切换、旧健康响应误判、迁移目标与兼容代际拒绝、失败回退、回退失败保留记录、并发锁、切换时 SIGKILL 后恢复、候选损坏恢复和不兼容数据库禁止回退。

这些测试证明本机工具的行为，不代表已经完成生产主机安装、低中断时延指标或长期稳定性验收。实际发布应记录停止和就绪时间、HTTP 错误率、Agent 连接与账务一致性，并使用 [acceptance.md](acceptance.md) 记录规定时长的稳定运行证据。
