# 生产部署与入口验收

当前仓库提供控制端/Agent 的 systemd 模板和本机发布工具；`compose.yaml` **只启动开发 PostgreSQL**，不能作为完整生产部署。不要将开发 `.env.local` 改一个模式开关后直接上线。以下方案适用于同机 Nginx HTTPS 入口、回环 API 与独立公开静态目录；其他负载均衡/CDN/容器拓扑需要重新验证代理来源。

## 准备实际主机

先确定正式域名、服务器/操作系统、数据库、SMTP、商户身份及备份位置。数据库应用身份应为专用 `NOSUPERUSER NOCREATEDB NOCREATEROLE` 登录角色，迁移仅需当前应用 schema 的 DDL 权限，备份/建库恢复使用受控运维身份。仓库开发 PostgreSQL 初始化账号 `xui` 是超级用户，不可直接作为生产应用账号。

使用实际应用连接在受控数据库控制台检查，三个权限字段都应为 `false`；不要将连接串或口令粘贴到聊天或命令行记录：

```sql
SELECT current_user, rolsuper, rolcreatedb, rolcreaterole
FROM pg_roles WHERE rolname = current_user;
```

跨主机访问还须核对角色成员关系及数据库授权，不能仅以三个字段为 false 推定已具备最小权限。

按[发布手册](releases.md)建立 `xui` 系统账号、私有 `/srv/xui` 发布树、`/var/lib/xui` 工作目录以及 `/etc/xui/control.env`。环境文件、MFA 密钥、商户/SMTP 凭据只在主机机密管理中提供，不写入公开目录或命令行。至少设置：

| 配置 | 同机入口要求 |
| --- | --- |
| `APP_MODE` | `production` |
| `PUBLIC_ORIGIN` | 实际 `https://panel.example.com`，无路径/尾斜线 |
| `HTTP_ADDR` | `127.0.0.1:8080`，防火墙不开放此端口 |
| `TRUSTED_PROXY_CIDRS` | 此模板固定为 `127.0.0.1/32`，不要信任整个云网段 |
| `MFA_KEY_FILE` | 已独立备份的私有固定密钥，见[二次验证](staff-mfa.md) |
| `REQUIRE_STAFF_MFA` | `true` |
| `DATABASE_URL` | 生产专用角色；跨主机必须验证数据库 TLS 证书 |
| SMTP 变量 | 实际服务配置，先运行[邮件检查](mail-acceptance.md) |
| 支付变量 | 显式启用已验收商户；未验收渠道保持关闭 |

应用启动会执行迁移。更新前完成一致快照备份和独立恢复演练，确认兼容范围；首次引入 MFA 安全代际需要维护窗口，不能自动退回 0.15。

## 独立发布公开静态资源

Nginx **不能读取整个 `/srv/xui` 发布树**。先校验发布清单，只将 `releases/<版本>/web/` 的已构建内容复制到 `/var/www/xui/releases/<版本>/`，公开目录 `0755`、普通文件 `0644`，建议由主机管理员拥有、Nginx 只读。不要复制环境、私钥、数据库、Agent 身份或 `release.json`。

可以使用受控 `rsync`，明确替换版本；不要对私有发布树执行递归开放权限：

```sh
release_id=0.16.0-example
sudo install -d -m 0755 /var/www/xui/releases/"$release_id"
sudo rsync -a --chmod=D755,F644 --chown=root:root \
  /srv/xui/releases/"$release_id"/web/ /var/www/xui/releases/"$release_id"/
```

首次发布前将 `/var/www/xui/current` 指向这份公开目录。后续发布先将旧公开目录中带内容哈希的 `assets/` 文件合并到新目录（已存在文件不覆盖），再以同目录临时符号链接加原子重命名切换 `current`。保留旧哈希资源和上一份 index 供缓存页面与回退使用，按实际保留期清理；不能先删旧目录再复制。控制面与前端切换分别核验，发布工具的 API readiness 不代表前端已经更新。

## HTTPS 入口

模板为 [xui.conf.example](../deploy/nginx/xui.conf.example)，放在 Nginx `http {}` 下的 `conf.d` 中。使用受支持的 Nginx（模板要求至少 1.19.4，实际应选仍有安全维护的版本），先替换所有 `panel.example.com`、证书路径和公开目录。证书必须由正式信任链签发，准备好续期及续期后的 reload；模板不自动申请证书。它没有开放 HTTP ACME 文件路径，可使用 DNS 验证或已有证书管理方案。

- 只接受固定域名，HTTP 固定跳转 HTTPS，未知 Host/TLS 名称拒绝。只允许 TLS 1.2/1.3，TLS 1.2 限制 ECDHE+AEAD 套件。HSTS 为该域一年，**不含 includeSubDomains/preload**；确认域名持续支持 HTTPS 后才能在公网启用，不能用临时证书冒充生产验收。
- `/api/v1/`（含订阅和支付通知）及 `/agent/v1/` 原路径转发，未知版本/其他路径返回 404。后台 `/api/v1/admin/` 仍由应用角色、邮箱验证及 MFA 保护；若需额外 IP/VPN 限制，应按实际人员网络配置，不能直接封掉正常后台接口。
- `/readyz`、`/healthz` 在公网返回 404，发布主机直接检查 `http://127.0.0.1:8080/readyz`。点文件和未知站点路径拒绝；当前前端使用根页面和 fragment，不需要通用 history fallback。未来增加路由时应显式允许前端路径，禁止将 API 404 改成 index HTML。
- 公网入口覆盖 X-Forwarded-For，只发送实际 `$remote_addr`；不信任访问者传入的链。若前面另有 CDN/负载均衡，不能照搬此拓扑；必须限制可连接源、验证真实客户端链，再修改边界配置。
- 原始请求体、Authorization 和微信验签头保持不变，不添加 JSON 包装，不对支付回调增加网页登录或 Origin 限制。普通上传最多 2MiB，原始账单专用接口 12MiB，通知 64KiB；应用仍执行自身更严格限制。20 秒上游读写超时应与控制端的 15 秒读写超时及支付外呼预算一起核对，API 不启用 gzip 或缓存。
- 只有带内容哈希的静态资源缓存一年，HTML 和 API 均 no-store；静态目录里的符号链接不被跟随。压缩只用于公开静态资源。

访问日志仅记录时间、对端、固定路由类别、方法、状态、字节和耗时，不记录 URI、query、Referer、User-Agent、正文或认证头。Nginx 内建请求错误日志会包含完整 URL，因此这些虚拟主机关闭请求错误日志，使用脱敏访问日志和应用结构化错误进行排障；启动/配置错误仍由 `nginx -t` 和服务管理器记录。不要另加默认 combined 日志、CDN 原始访问日志或未经脱敏的 APM。订阅令牌与易支付 GET 参数可能是凭据，日志保留与访问权限仍需管控。

安装配置前执行 `nginx -t`，只在通过后 reload；不要关闭正在运行的主机服务来试错。本仓库模板的本地合成证书验证记录不替代目标公网证书和真实回调验收。

## 可重复的本地入口测试

在 Linux、Docker 和 OpenSSL 已安装的独立验收机器上执行：

```sh
python3 scripts/test-production-proxy.py --pull
```

首次命令显式下载脚本内固定 SHA-256 的 Nginx 镜像。已有该镜像时可省略 `--pull`，脚本不会自动更新到浮动标签。测试不安装主机 Nginx、不读取生产配置或数据库，不使用真实域名证书，也不连接商户。它创建私有临时文件、本机合成 CA/证书和回环上游，用 `nginx -t` 检查原始 IPv4/IPv6 配置，再将运行时端口改为随机 `127.0.0.1` 监听。

34 项检查涵盖真实 TLS 证书/主机名验证、错误 CA 与 SNI 拒绝、固定跳转、私有路径拒绝、API 404、Agent/订阅/通知原始路径及数据、转发身份覆盖、上传上限、静态缓存与符号链接拒绝、上游故障，以及访问和错误日志无测试令牌。所有容器都有本轮随机名称和所有权标签，退出时仅移除本轮创建且标签相符的容器；失败不会重启或删除已有服务。清理失败返回非零，应检查 `com.xui.production-proxy-test` 标签的临时容器后处理。

这些结果只证明模板在合成环境中的行为。实际公网 DNS、CA 链、证书续期、IPv6 可达性和 CDN/负载均衡后的客户端身份仍需在目标环境另测。

## 放行条件

正式域名需实测：HTTP 跳转、可信 TLS 链/域名/续期、Secure/HttpOnly 会话、管理员邮箱与 MFA、代理后的限流地址、用户购买/订阅、Agent HTTPS 同步以及原始支付通知。检查未知 Host、公开 readiness、私有文件和不存在 API 都不能暴露页面/内部资料，检查订阅及易支付日志中没有令牌或签名。

记录真实收件箱到达、目标商户付款/查询/退款的批准范围、多日真实节点稳定运行、磁盘与队列积压告警，以及目标规模 RPO/RTO 和更新/回退中断时间。没有这些环境和证据时，只能报告代码及本地验证结果，不能签署正式上线通过。
