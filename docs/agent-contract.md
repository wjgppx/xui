# 节点协议 v1

当前契约由 `internal/agentwire` 类型和 `api/openapi.json` 描述。已实现注册、含授权的完整期望状态、心跳、应用结果和不可变流量批次。

节点通过 HTTPS 主动请求控制端。注册令牌限时有效，相同节点凭据允许幂等重试丢失的应答。节点 Bearer 身份与 Web Cookie 隔离，只能读取和报告自己的状态。后台提供凭据轮换、撤销及管理操作审计。

| 接口 | 已实现行为 |
| --- | --- |
| POST /agent/v1/enroll | 用注册令牌和已在本地持久化的节点凭据建立身份 |
| GET /agent/v1/desired-state | 完整版本化 bindings、有效权益用户和最迟 60 秒到期的授权租约 |
| POST /agent/v1/heartbeat | Agent 版本、单调已读取版本和最近心跳 |
| POST /agent/v1/applied-state | 当前期望版本的 applied/failed、脱敏错误码、实际进程实例 ID |
| POST /agent/v1/usage-batches | 固定 ID/序号/内容的流量批次，验证历史归属并原子入账，返回批次 ID 与摘要 |

期望状态包含 `schema_version=1`、`node_id`、`revision`、`lease_expires_at`、`bindings`。每个绑定包含稳定 ID、协议、监听 IP/端口、TLS 域名、本地证书引用和客户端列表；每个客户端包含平台用户 ID、权益周期 ID、协议凭据及权益结束时间。支持 VLESS TCP+TLS、Trojan TCP+TLS，不接收任意 Xray JSON 或私钥。

快照内容稳定时不因续租推进版本；业务事件也可推进节点期望版本来提示协调。授权因支付、组成员/启用状态、权益撤销、到期或数据库额度状态改变而变化时，重新计算快照。内核内容未变时，Agent 确认新版本而不重启进程。旧版本应用结果被拒绝，版本未被心跳确认读取时也不能报告应用成功。`failed` 不推进已应用版本。当前进程按整份配置重启，因此结果为整个节点，而不是逐绑定部分成功。

后台配置采用完整替换和乐观版本校验，支持空配置停用代理。TLS 文件名只允许安全的引用名称。节点身份重置清空旧的观测/应用状态；节点撤销后所有新控制请求拒绝。

流量批次身份为认证得到的 node_id + batch_id，另对 node_id + core_instance_id + sequence 去重。载荷包含 started_at、ended_at 和按绑定/用户/权益周期划分的上行、下行原始字节。类型允许最多 1000 条目，同时受 64 KiB JSON 请求限制；采样区间为正且不超过 2 分钟。

控制端按历史连续授权区间和权益归属验证所有条目，允许 30 秒时钟/测量边界容差。批次证据、明细和原权益周期 used_bytes 在同一事务提交；超出套餐额度的已发生字节不截断。相同 ID/内容重投返回原确认，相同 ID 不同内容或重复内核序号返回 409，未获历史授权返回 403。确认包含 batch_id 与 UTC 规范化类型载荷的 SHA-256 digest，条目顺序参与摘要。

Agent 的 SQLite 游标与不可变待发送批次同事务提交；只有确认 ID 和摘要都匹配才删除本地批次。正常停机前最后采样仍不消除查询到停止之间的窗口，强制终止或崩溃也可能丢失最后一次成功采样后的字节。完整口径见 [traffic-metering.md](traffic-metering.md)。

节点进程管理细节、租约和中断语义见 [node-agent.md](node-agent.md)。VMess 已转为后续兼容选项，依据见 [xray-capability-report.md](xray-capability-report.md)。

后台 BindingConfig 的 access_mode 为 manual 或 plan，缺省/空值保持 manual。此字段属于控制端授权决策，不改变 Agent 的绑定传输协议；下发的 clients 已经过当前有效权益和组关系筛选。业务事件投递只推进期望版本，不等于 Agent 已读取或内核已应用，详见 [access-and-events.md](access-and-events.md)。
