# XUI

独立开发的订阅服务平台。当前增量实现用户门户、运营后台、开发支付到权益，节点 VLESS/Trojan TLS 配置应用、持久化流量入账、节点组/套餐授权、业务事件投递，以及账号安全、后台分权和邮件投递，采用 Go、PostgreSQL、Vue 3、TypeScript 和 Naive UI。

完整规划见 [开发方案](docs/development-plan.md)，实际进度见 [交付记录](docs/delivery.md)，交易与周期规则见 [业务规则](docs/business-rules.md)，本轮结果见 [验证记录](docs/verification.md)。

## 本地开发

需要 Go 1.27.3、C 编译器（GCC）、Node.js 24、npm、Docker Compose。Agent 的 SQLite 驱动使用 CGO，构建时要求 `CGO_ENABLED=1`；驱动内附 SQLite，不需要系统 SQLite 开发包。云工作区可运行安装脚本；其他平台先安装对应 Go 并设置 `XUI_GO_BIN`。

```bash
bash scripts/install.sh
docker compose --env-file .env.local up -d --wait db
bash scripts/api.sh
```

另一个终端运行前端：

```bash
cd web
npm run dev
```

前端监听 `127.0.0.1:5173`，API 监听 `127.0.0.1:8080`。前端开发服务器代理 API。`GET /healthz` 验证数据库可用性。`.env.local` 自动生成一次，使用随机本地数据库密码，禁止提交到 Git。更改前端域名或端口时同步更改 `PUBLIC_ORIGIN`。

注册自己的账号即可查看套餐、创建订单、确认测试支付、查看已开通及续费权益。成为管理员需要在已有账号注册后执行：

```bash
bash scripts/api.sh --promote-admin your-email@example.com
```

角色变更会使该账号的已有会话失效，重新登录后出现相应后台入口。管理员可以查看订单、创建套餐、编辑节点入站、节点组和套餐授权，并管理账号角色。没有硬编码的用户或管理员密码。

“账号安全”提供验证邮箱、修改密码和开发收件箱；登录窗口提供忘记密码。开发模式不会发送外部邮件，可在自己的开发收件箱打开测试链接。生产模式要求邮箱验证和已配置的 SMTP 服务，配置与权限范围见 [账号安全与邮件](docs/security-and-mail.md)。

## 验证

```bash
bash scripts/go.sh build -o bin/xui-agent ./cmd/agent
bash scripts/test.sh
bash scripts/go.sh vet ./...
python3 scripts/install-xray.py
python3 scripts/install-mihomo.py
bash scripts/test-xray.sh
cd web
npm run build
npx playwright install chromium
npm run test:e2e
```

浏览器测试要求 API 和前端正在运行。如果机器已有 Chromium，可使用 `PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/chromium npm run test:e2e`；受限云环境下载浏览器时将 `PLAYWRIGHT_BROWSERS_PATH` 设置为可写目录。

Go 集成测试使用 `TEST_DATABASE_URL`，每次创建独立 schema，结束后只清理自己的 schema。缺失数据库配置会失败而不是跳过。`.github/workflows/ci.yml` 定义相同的数据库、构建和浏览器检查。

## 接口与限制

实际 API 契约见 [api/openapi.json](api/openapi.json)，节点协议见 [agent-contract.md](docs/agent-contract.md)。

- `APP_MODE=development` 才提供测试支付和自己的开发收件箱；`production` 下这些入口返回 404，且要求 HTTPS Origin、Secure Cookie、SMTP 配置和邮箱验证。
- 测试支付没有连接支付机构沙箱，不产生真实交易。已实现邮箱验证、找回/修改密码、客服/财务/运维/管理员分权及 PostgreSQL 共享限流；真实支付、退款和交易核对尚待实现。
- 已支持权益过滤与 VLESS/Trojan TCP + TLS 配置应用。整份配置重启会中断连接，失联在最长 60 秒授权租约到期后停机。已接入 SQLite 流量待发送批次、服务端去重与额度入账。采样/上报有延迟，最终采样到停止或崩溃仍可能丢失尚未采集的字节；已支持订阅导出与业务事件 outbox 消费；事件投递完成和内核应用状态分别展示。
- 订单与管理员列表当前展示最近 100 条；分页、订单关闭、更多运营操作后续实现。
- 当前只能作为开发版本使用，生产上线须完成交付记录中的节点能力、交易核对、权限和运维验收。
- 当前直接按 TCP 对端地址限流，不信任 `Forwarded` 或 `X-Forwarded-For`。反向代理后的用户会共享代理 IP 的额度；可信代理配置与规模验收仍待实现。

停止数据库使用 `docker compose --env-file .env.local stop db`，再次启动会保留数据。不要用带 `-v` 的删除命令清理已有业务数据。

## 节点接入

后台已提供节点管理，支持身份注册、轮换/撤销、入站配置、账号授权和实际应用状态。运行环境为 Linux。启动方式见 [节点 Agent](docs/node-agent.md)，计量口径与故障边界见 [流量计量](docs/traffic-metering.md)，本地 Xray 实测见 [能力报告](docs/xray-capability-report.md)。

## 客户端订阅

用户门户提供“我的订阅”，可生成和撤销订阅链接、重置自己的代理凭据，下载 URI/Base64/Mihomo/Xray 配置。管理员需先发布连接地址/端口，节点确认应用后才进入订阅；详情见 [订阅说明](docs/subscriptions.md)。

## 套餐与节点组

“权限管理”可维护节点组、配置新订单的套餐授权并查看/重试失败业务事件。套餐组在下单时形成快照；组成员与启用状态影响既有权益的运行时可用节点。入站可选择手工兼容模式或套餐节点组模式。控制端正常启动自动运行事件消费者；详情见 [授权和事件](docs/access-and-events.md)。
