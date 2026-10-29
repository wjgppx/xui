# 真实邮件投递验收

生产使用配置的真实 STARTTLS SMTP 服务，账号邮件仍走 PostgreSQL 事务发件箱。工具先帮助检查传输和认证，再由操作者明确发出测试邮件；本轮自动化只连接本机临时 TLS 服务，没有向外部收件人发送邮件。

在私有环境变量中配置 `SMTP_ADDR`、`SMTP_FROM`、`SMTP_TLS_SERVER_NAME`（可省略）、成对的 `SMTP_USER` 和 `SMTP_PASSWORD`，不要把密码写进命令行或仓库。配置规则见[账号安全与邮件](security-and-mail.md)。

```bash
bash scripts/go.sh build -o bin/xui-mailcheck ./cmd/mailcheck
./bin/xui-mailcheck
```

默认只验证连接、STARTTLS 证书和可选 AUTH，不执行 MAIL、RCPT、DATA，不发送邮件。成功输出有限 JSON，例如 `tls_verified=true`、`authenticated=true`、`message_submitted=false`；认证字段为 false 也可能是已授权的匿名中继，需按服务商要求确认。证书错误、没有 STARTTLS、认证失败或超时返回失败，不打印服务商原始响应或凭据。

操作者准备好接收邮箱后，明确执行：

```bash
./bin/xui-mailcheck --send-test --to operator@example.com
```

成功的 `message_submitted=true` 只表示 SMTP 已接受一封测试邮件。需要人工在目标收件箱确认到达时间、垃圾箱归类、SPF/DKIM/DMARC 结果、发件域与服务商发件授权；如未到达，查服务商投递与退信记录。Message-ID 使用配置的发件域和稳定随机任务编号，不自动签署 DKIM；域名签名通常由授权服务商处理。

随后在生产测试账号分别申请验证及重置邮件，确认链接指向正确的 HTTPS 站点、替换后的旧令牌失效、已用链接拒绝、重置后所有会话失效而已启用的 MFA 保留。记录服务商、脱敏邮箱域、申请/SMTP 接受/收件时间及结果；证据不要包含完整链接或令牌。服务商接入、收件箱到达、退信告警和域名认证结果齐备前，真实投递验收保持待完成。

SMTP 与数据库提交之间仍可能出现重复尝试，不能承诺邮件恰好投递一次。邮件适配器、队列重试与已接受 DATA 后断开 QUIT 的边界见[账号安全与邮件](security-and-mail.md)。
