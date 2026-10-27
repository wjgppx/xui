# Xray 能力实验：2026-10-26

官方版本 Xray 26.10.21，提交 `1b1647cd9ddf0abc34b981d4611fc03bcac1ca74`，Linux amd64。官方发布包 SHA256 为 `0eec3ed4163c273d8c8bf821f7d219e257921cf584aa3fb970715e458e631978`，已校验后执行。

实验脚本与配置独立编写，只建立回环 Xray 服务端、SOCKS 客户端及本地 TCP echo 服务。当前内核默认阻止回环目标，测试仅为本次 echo 服务的 127.0.0.1/32 和具体 TCP 端口配置显式允许规则，不作为生产出站配置。

| 实测行为 | VLESS TCP | VMess TCP |
| --- | --- | --- |
| 热添加用户并核验用户数量 | 通过 | 通过 |
| 真实代理连接及完整载荷校验 | 通过 | 通过 |
| 上传/下载计数均大于零，重复查询不清零 | 通过 | 通过 |
| 删除用户后拒绝新连接 | 通过 | 通过 |
| 删除用户后已有连接仍可传输 | 是 | 是 |

因此不能仅靠删除用户宣称即时断流或硬额度生效。累计计数可用于后续计量，但没有证明内核崩溃时零丢失、跨节点计量或周期切分正确。

当前官方内核运行时提示 VMess 已弃用。后续新开发优先 VLESS + TLS、Trojan，VMess 保留兼容测试，完整方案中的早期协议顺序据此调整。

另一个实测发现：`xray api adu` 配置构建失败可能只打印错误并返回零退出码，因此必须核验用户数量及实际连通。

```bash
python3 scripts/install-xray.py
python3 scripts/xray-capability-check.py
python3 scripts/xray-capability-check.py --protocol vmess
```

范围仅为回环 TCP；TLS、WebSocket、Trojan、公网、长期稳定性及吞吐尚未实测。测试凭据随机生成，临时文件与自己启动的进程在结束时清理。

来源：[官方发布](https://github.com/XTLS/Xray-core/releases/tag/v26.10.21)、[官方管理 CLI](https://github.com/XTLS/Xray-core/tree/v26.10.21/main/commands/all/api)、[出站配置](https://github.com/XTLS/Xray-core/blob/v26.10.21/infra/conf/freedom.go)。


## 2026-10-27：平台实际 TLS 接入

新增 `scripts/test-xray.sh`，使用独立数据库 schema、真实控制 API、真实 Agent 协调器和官方 Xray 服务端/客户端。VLESS 与 Trojan 都完成了带证书验证的 TCP + TLS 数据传输；错误用户凭据、错误 CA 被拒绝。

本轮还验证了配置预检失败保留旧服务、占用端口时回退、回退进程实例 ID 更新、移除用户时通过整进程替换关闭旧连接、租约到期停机及续租后恢复、空配置停机，以及配置校验延迟不会延长旧租约。详见 `internal/integration/proxy_test.go`。

这是与前一轮动态删除用户行为不同的实际策略：当前 Agent 使用整份配置替换进程，因此会中断所有现有连接。动态用户 API、累计计数的前期实验仍不能替代下一阶段的持久化流量上报和重启丢数验证。
