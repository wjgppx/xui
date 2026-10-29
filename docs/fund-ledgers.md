# 独立资金流水观察与银行记录核对

控制端0.15 / 数据库迁移v15增加独立的资金流水观察账本。管理员和财务可导入自行整理的商户或银行日流水，检查批内余额，再选择两个明确账户的批次核对提现记录与银行记录。它与[交易毛额对账](merchant-statements.md)、[费用与渠道金额比较](statement-financials.md)分别保存，不会确认付款、发起提现或退款、改变订单及权益。

本轮只支持下述规范CSV。没有接入支付宝/微信官方资金流水原始格式、`fundflowbill`下载或银行API；手工来源未验签，观察匹配也不能作为渠道已确认到账的证明。此功能不要求启用`PAYMENT_ACTIONS_ENABLED`或`BILL_DOWNLOADS_ENABLED`。

## 账户和批次范围

每份导入明确填写渠道、账户别名、北京时间业务日期、覆盖范围以及期初和期末观察余额。渠道为`alipay`、`wechat`或`bank`，币种固定为CNY。账户别名使用小写英文字母开头、最长64位的英文字母/数字/下划线/连字符，例如`alipay_main`、`bank_ops`；请用内部别名，不填写真实银行卡号、密钥或下载地址。

一个别名首次导入后固定对应渠道和CNY，不能在后续文件中改为另一渠道；冲突返回409。账户别名及对方账户映射都是操作者声明，系统不验证它们与真实商户或银行账户的归属。对方别名允许先填写、后导入另一方文件，但必须格式正确且不能等于本账户。

每个批次是独立、不可变的观察快照。相同规范内容及元数据重复导入复用原批次，不重复审计；修正文件形成新批次。相同账户或同一天的多份部分文件、重叠文件及修正版不会相加成当前余额；跨批次相同`entry_id`也不被累加或默认为新的资金流动。列表只显示所选批次声明的余额，不能据此得出当前银行余额。

`partial`是默认覆盖范围。界面选择`full`时须明确声明文件包含该日完整范围，但这种声明与余额算术自洽都不能证明来源完整、真实或已验签。本轮只核对批内连续性，不自动拼接跨日/跨批次余额。

## 规范CSV与金额

导入请求是单个严格JSON对象：

```json
{
  "provider": "alipay",
  "account_key": "alipay_main",
  "date": "2026-10-28",
  "coverage": "partial",
  "opening_balance_yuan": "100.00000",
  "closing_balance_yuan": "80.00000",
  "csv": "sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key\n1,merchant-out-1,2026-10-28T15:00:00+08:00,withdrawal,debit,20.00000,80.00000,transfer-A,bank_ops\n"
}
```

CSV表头严格为：

```csv
sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key
```

限制为UTF-8，可带BOM，最多1MiB、2000条；整个JSON请求最多2MiB。禁止空白记录、控制字符、未知字段、错列和重复`entry_id`。只保留规范流水字段，不保存原始文件、文件名、下载URL、账户密钥或交易描述。

`sequence`须按文件物理顺序从1连续递增；系统不按交易号或时间重新排序。`occurred_at`必须是带时区的RFC3339时间，属于所选北京时间日期，按顺序不递减，同一时间可以有多笔；保存时统一为UTC微秒精度。日期支持2000至2099年。

金额使用元，输入最多25位整数、5位小数，禁止正号、前导零、空白、千分位、科学计数法和额外小数位。金额不经浮点或分金额`int64`缩放，输出固定五位小数；`amount_yuan`必须大于零，借贷由`direction=credit|debit`明确表达。观察余额可为负数，负零规范为零。计算合计及期望余额最多29位整数、5位小数。

| 流水类型 | 商户账户 | 银行账户 |
| --- | --- | --- |
| `payment` | 仅`credit` | 不支持，使用`other`表达其他银行流入/流出 |
| `refund` | 仅`debit` | 不支持，使用`other` |
| `withdrawal` | 仅`debit`，参与对银行记录的核对 | 仅`debit`，不作为商户到账记录 |
| `settlement` | 不支持 | 仅`credit`，参与商户提现核对 |
| `deposit` | 仅`credit` | 仅`credit` |
| `fee`、`adjustment`、`other` | 可`credit`或`debit` | 可`credit`或`debit` |

只有商户`withdrawal/debit`与银行`settlement/credit`必须填写`transfer_id`和`counterparty_account_key`，其他行这两列必须留空。`entry_id`和非空转账引用是最长128位ASCII标识，可含字母、数字及`._:-`，首字符必须是字母或数字。转账引用可以重复导入保存，但重复候选在对比时会明确标为有歧义，不会任选一条。

## 余额连续性

格式正确的流水即使存在余额差异，也会保存观察及有限差异代码，不会静默修改余额：

- 从期初观察余额累计每笔借贷得到`expected_balance_yuan`，与本行`balance_after_yuan`不同则记录`running_balance_mismatch`。
- 用上一行**报告余额**加本行借贷得到`step_expected_balance_yuan`，首行使用期初余额；不同则记录`step_balance_mismatch`。它帮助定位局部断点，与累计检查分别展示。
- 期初加全部贷记、减全部借记得到`expected_closing_balance_yuan`，与声明期末不同则记录`closing_balance_mismatch`。
- 有流水时，最后一笔报告余额与声明期末不同，另外记录`closing_entry_mismatch`。

`balance_difference_count`统计至少带一个行级代码的行数，不把同一行的两个代码当两笔差异；期末提示保存在`summary_codes`。空文件可只有表头，期初与期末不相等时仍报告期末差异。结构、顺序、标识和金额精度错误则整批拒绝。

## 商户提现与银行记录比较

在“资金流水”页面选择一份支付宝/微信商户批次和一份银行批次。可以选不同日期，以核对跨日记录。报告同时绑定两个账户别名和两个不可变批次：

1. 商户侧只选择提现行，且`counterparty_account_key`等于所选银行别名。
2. 银行侧只选择入账行，且对方别名等于所选商户别名。
3. 在这个明确范围内按相同`transfer_id`比较，金额必须精确相同；不按近似金额、时间窗口或日期猜配，也不默认扣除手续费。

例如，前述`alipay_main`提现20元到`bank_ops`，可与次日银行批次中的这一行比较；该银行批次期初50元、期末70元：

```csv
sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key
1,bank-in-1,2026-10-29T09:00:00+08:00,settlement,credit,20.00000,70.00000,transfer-A,alipay_main
```

| 状态 | 含义与代码 |
| --- | --- |
| `matched` | 双方各有一条候选，引用及金额相同，仅表示观察匹配 |
| `difference` | 双方各一条但金额不同：`amount_mismatch` |
| `unmatched` | 本次范围缺少一方：`bank_record_missing`或`merchant_record_missing` |
| `ambiguous` | 任一方同一引用有多条候选：`duplicate_transfer_id`；全部候选保留，不选第一条、不合并猜配 |

“本次银行范围未列出”需要继续检查跨日、延迟及所选文件范围，不能直接解释为未到账或资金丢失。提现费是否另列或合并由实际流水决定，系统不使用交易账单服务费推断提现差额。

报告另外统计对方账户不同的转账候选（`*_out_of_scope_count`）及非转账行（`*_other_count`），它们不计入当前配对结果。来源是部分批次或有余额差异时，保留`merchant_partial`、`bank_partial`、`merchant_balance_issues`、`bank_balance_issues`提示；即使所有候选相等也不能清除来源警告。零候选不等于全部到账。

报告是非消耗式的比较快照，不建立全局唯一结算关系。同一流水可能出现在历史不同批次/范围的报告中；它不会被标记为已使用资金，也不会因新报告而改变原观察。重复提交相同批次对及比较版本会复用原报告；修正来源形成新批次后可生成另一份报告。

## 权限、接口与历史

新增`funds.read`、`funds.import`、`funds.reconcile`，仅管理员和财务拥有。生产邮箱验证、账号会话安全版本、可信Origin和JSON规则继续生效。导入和对比各自按用途限制直接对端IP及当前用户各5次/分钟。

| 方法与路径 | 请求与结果 |
| --- | --- |
| `GET /api/v1/admin/fund-ledgers` | 最近100份导入摘要，`{items}` |
| `POST /api/v1/admin/fund-ledgers` | 前述严格导入JSON；返回保存或复用的批次及至多2000行 |
| `GET /api/v1/admin/fund-ledgers/{id}` | 指定批次、余额检查及完整规范流水 |
| `GET /api/v1/admin/fund-reconciliations` | 最近100份不可变报告摘要，`{items}` |
| `POST /api/v1/admin/fund-reconciliations` | `{merchant_batch_id,bank_batch_id}`；返回保存或复用的报告 |
| `GET /api/v1/admin/fund-reconciliations/{id}` | 指定报告及至多4000个引用分组 |

所有接口不接受查询参数。列表只读摘要，明细接口一次读取其有界范围，界面每页100条；已知较早的批次或报告ID仍可读取。账户注册、批次、报告及审计分别随对应操作原子提交；审计失败不会留下半份证据。存储表阻止更新或删除既有观察和报告。

请求或CSV错误返回400，未登录401，权限/邮箱/会话版本不足403，未知批次或报告404，账户渠道冲突、错误批次组合或并发冲突409，限流429，数据库不可用503。错误只包含有限字段与原因，不回显导入内容。完整响应字段见[OpenAPI](../api/openapi.json)。

## 后续验收

本轮使用独立合成数据和本地PostgreSQL验证，不代表真实银行或商户验收。下一步需对接明确版本的官方资金流水档案及下载协议，并使用目标商户/银行文件验证身份、日期、提现、余额和到账语义。随后仍需完成真实商户/官方沙箱、管理员二次验证、可信代理与限流规模、备份恢复及故障/升级回退等交付目标。实际测试结果见[验证记录](verification.md)。
