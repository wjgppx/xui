# 原始账单费用与渠道金额核对

控制端0.14 / 数据库迁移v14增加原始来源的费用和渠道金额观察，以及人工预期CSV比较。它与[毛额收款/退款对账](merchant-statements.md)分开保存，不会确认付款、执行退款、变更订单或权益，也不会建立资金操作任务。原始上传和官方下载均可产生金额观察；下载总开关关闭时，手工原始导入及本页比较仍可使用。

## 观察来自哪一个文件

费用数据绑定不可变的 `source_id`，不只绑定规范毛额账单。两个原始文件可能具有相同支付单号、交易毛额和日期，但手续费不同；它们继续复用同一规范账单，并保留各自来源及费用观察，不能相互覆盖。

每个来源只保存一份 `native-financials-v1` 观察。原始文件仍只在内存中校验，不保存文件、文件名、下载地址、商户密钥或个人商品信息。观察只保留支付/退款标识、费用、渠道金额、合计和有限汇总提示。

迁移不从既有毛额猜测费用。历史来源及标准CSV来源可能返回 `available=false`，此时版本和口径为空字符串、行与汇总提示为空数组、金额合计为null；这与手续费为零不同。若仍持有原始文件，可明确重新导入同一文件，按来源摘要复用来源并首次补充观察及审计。既有账单、来源和历史报告不被改写；下载已成功的任务也不会因本次升级自动重新下载。

## 金额口径与精度

| 来源 | `basis` | 费用 `fee_yuan` | 渠道金额 `channel_amount_yuan` |
| --- | --- | --- | --- |
| 微信SUCCESS档案 | `wechat_settlement_order` | 手续费 | 应结订单金额 |
| 微信REFUND档案 | `wechat_refund` | 手续费，保留原符号 | 退款金额，保留原符号 |
| 支付宝经典25列档案 | `alipay_merchant_received` | 服务费 | 商家实收 |

这里的字段是元金额，与既有毛额接口的正整数分 `amount_minor` 不同。费用与渠道金额使用精确十进制字符串，规范输出固定五位小数，不经过浮点数、四舍五入到分或 `int64` 缩放。单行最多25位整数、5位小数；合计最多29位整数、5位小数。输入允许负数、零及1至5位小数，禁止正号、前导零、空白、千分位、科学计数法和超过五位的小数；负零规范为 `0.00000`。

支付宝空服务费或空商家实收保持 `null`，不能当成零；任一行未知时，该字段整批合计也为 `null`，不会展示一个不完整的部分合计。微信档案要求明确金额。退款的费用符号直接采用源文件，不因退款类型再翻转一次。

微信应结订单金额可能因非充值券与订单毛额不同，退款金额也可能与申请退款毛额不同。支付宝商家实收不被推断为“毛额减服务费”。本轮不保存或计算分润、不推断计费费率、不套用净额恒等式，也不计算银行到账金额。账单日期与退款成功日期均不等同银行结算日期。

微信文件的费用汇总与对应渠道金额汇总另外和明细合计比较。不相等时保留 `fee_total_mismatch` 或 `channel_amount_total_mismatch` 提示，供人工检查；它们不会把结构正确、毛额汇总通过的整份交易账单拒绝，也不会修改原毛额差异报告。未知表头、无汇总、错误毛额、坏压缩包、商户不一致和无效金额仍按原始格式规则拒绝。

## 上传明确预期金额

在商户账单详情中选择一个来源，读取“费用与渠道金额”，再上传自行核定的预期CSV。预期可依据商户合同、独立账务记录或人工检查结果整理；系统不从账单费率、订单原价或默认0.6%猜测预期。

表头严格为：

```csv
payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan
```

收款示例，退款列留空：

```csv
payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan
payment-A,,0.11405,18.01000
```

退款示例，保留预期费用的明确符号：

```csv
payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan
payment-A,refund-A,-0.01000,18.01000
```

这些是独立构造的格式样例，不代表任何商户合同或费率计算结果。预期两项金额都必须明确填写，不能用空白表示“不检查”。CSV要求UTF-8，可有BOM，最多1MiB、2000条；收款按 `payment_id` 唯一，退款按 `refund_id` 唯一并保留对应 `payment_id`。标识沿用规范账单的ASCII格式，禁止重复身份和空行。仅有表头可表示本次没有提供预期记录，不能据此把来源记录标成匹配。

比较覆盖来源记录与预期记录的并集，最多4000条：

| 结果 | 含义 |
| --- | --- |
| `matched` | 本行费用和渠道金额均已知，且与明确预期分别相等 |
| `difference` | 存在缺失观察、缺失金额或金额差异；需要核查 |
| `unassessed` | 来源有记录，但本次没有对应预期，不能判为匹配 |

有限差异代码包括 `missing_reference`、`missing_observation`、`identity_mismatch`、`fee_mismatch`、`channel_amount_mismatch`、`fee_unavailable` 和 `channel_amount_unavailable`。退款身份匹配时还须核对对应支付身份；`payment_id` 保留来源支付单号，`expected_payment_id` 单独保留预期支付单号，没有预期时为空字符串，防止身份不一致时丢失预期证据。汇总提示与逐行匹配分别展示，某行匹配不表示文件汇总或资金到账已确认。

每次比较保存不可变报告、规范预期摘要、逐行结果及审计。同一来源上传语义相同的预期文件会复用原报告；改变顺序、CSV引用方式或金额小数位写法不会制造新版本。不同预期内容生成新版本，不覆盖旧报告。最近20个版本在来源详情中展示；已知报告ID可读取历史报告。界面按每页100条展示已读取结果，接口本身一次返回该报告至多4000条，不接受分页查询参数。

## 权限与接口

读取要求 `statements.read`；提交预期比较要求 `statements.reconcile`，仅管理员和财务拥有。生产邮箱验证、可信Origin、JSON和会话安全版本继续生效。比较按用途分别限制直接对端IP和当前用户各5次/分钟。没有额外商户网络请求，也不要求启用资金操作或官方下载。

| 方法与路径 | 请求与结果 |
| --- | --- |
| `GET /api/v1/admin/statements/{id}/sources/{sourceID}/financials` | 来源观察、可用性、合计、汇总提示及最近20个比较版本 |
| `POST /api/v1/admin/statements/{id}/sources/{sourceID}/financials/reconcile` | 严格JSON `{csv}`，请求最多2MiB；返回保存或复用的报告 |
| `GET /api/v1/admin/statements/{id}/sources/{sourceID}/financials/runs/{runID}` | 指定来源的不可变比较报告及逐行结果 |

三个接口都不接受查询参数。来源、报告必须属于路径中的账单和来源，未知或错误归属返回404；来源尚无金额观察时提交比较返回409。无效文件或请求400，未登录401，权限/验证/会话失效403，限流429，数据冲突409，数据库不可用503。错误只返回有限字段与错误说明，不回显文件内容、商户地址或凭据。完整字段见[OpenAPI](../api/openapi.json)。

## 验收边界与后续

本轮使用独立合成账单和本机协议响应验证精度、来源隔离及比较，不代表真实商户验收。原始下载的完整性标签只说明文件获取链路；人工预期也不能替代渠道验签、付款证明或银行流水。

后续需将资金流水账单作为独立账本接入，并处理结算日、账户余额、提现及银行到账核对；还需完成目标商户原始文件、计费合同、官方沙箱和真实商户端到端验收。微信交易账单与资金账单是不同接口，本轮没有接入 `fundflowbill`。

字段语义依据：

- [微信官方退款金额模型](https://github.com/wechatpay-apiv3/wechatpay-java/blob/1dab7bec717989e4a4f006d2469c3eebe9eabba7/service/src/main/java/com/wechat/pay/java/service/refund/model/Amount.java)：区分毛额、应结金额、用户现金退款和手续费退款。
- [微信官方账单服务](https://github.com/wechatpay-apiv3/wechatpay-java/blob/1dab7bec717989e4a4f006d2469c3eebe9eabba7/service/src/main/java/com/wechat/pay/java/service/billdownload/BillDownloadService.java)：交易账单与资金流水账单分开申请。
- [支付宝官方查询响应](https://github.com/alipay/alipay-sdk-java-all/blob/74b7a70f34fe9c7cca6b0ec429c7e955ed467c3e/v2/src/main/java/com/alipay/api/response/AlipayTradeQueryResponse.java)：实付、实收、优惠及结算币种金额分别表达，不能据此推定经典CSV三列的扣费恒等式。

测试结果由[验证记录](verification.md)单独记录。
