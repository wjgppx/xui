# 备份与恢复验收

`scripts/backup.py` 使用部署中 PostgreSQL 服务自带的 `pg_dump` / `pg_restore`，生成私有 custom-format 归档与 SHA-256 清单。控制端不提供备份下载或恢复 HTTP 接口。该工具只支持新建恢复数据库：已存在的目标会直接失败，不执行 `--clean`、不覆盖现有数据库。

## 操作方式

先创建由当前操作者拥有的私有目录。以下命令假设仓库根目录、现有 `compose.yaml` 与私有 `.env.local`；其他部署可通过 `--compose-file`、`--env-file`、`--service`、`--user` 指定。数据库名仅接受小写字母开头的字母、数字和下划线，不能传连接 URI。不要把数据库口令放在命令行。

```sh
install -d -m 700 .local/backups
python3 scripts/backup.py backup --database xui --output .local/backups/control-001.dump
python3 scripts/backup.py verify --input .local/backups/control-001.dump
python3 scripts/backup.py restore-drill --input .local/backups/control-001.dump
```

`verify` 仅检查本地格式、权限、大小与摘要，不连接数据库。`restore-drill` 实际创建随机临时数据库、原生恢复、核对证据，最后删除自己创建的库。恢复失败也清理该临时库；清理失败会返回失败，操作者必须处理遗留库。数据库账号需具备读取源库及创建/删除演练库的权限，放在受控运维环境，不能给 Web 请求使用。

保留恢复结果时使用明确的新名称：

```sh
python3 scripts/backup.py restore-new --input .local/backups/control-001.dump --database xui_recovered_001
```

此命令不会切换控制端连接。先关闭恢复环境中的邮件发送、支付操作与账单下载消费者，隔离控制端及 Agent 写入，完成下述核对，再按发布流程修改连接配置。恢复库应使用与归档兼容的 PostgreSQL 主版本和应用版本；工具不会下载数据库软件，也不执行数据库降级。原库保留供核对。

## 一致性与可核对证据

源库采用只读 `REPEATABLE READ` 事务导出的同一个快照，同时供证据查询与 `pg_dump` 使用，不要求停掉正常写入。证据包括业务表行数、约束、索引、触发器定义、关键财务/计量列总和，以及订单、支付、权益、用量、资金观察和账号/MFA 状态等表的行指纹。恢复后重新计算并逐项比较；不匹配即失败。指纹只用于恢复比较，不是支付到账证明或防恶意篡改签名。

归档和清单均为 `0600`，父目录必须由当前用户拥有且不可被组或其他用户访问；拒绝符号链接、硬链接输入和已有输出路径。临时文件写完并同步后原子发布，清单作为完成标记，缺少一侧的归档不能通过验证。SHA-256 只能发现与清单不一致的损坏；能同时修改二者的攻击者仍可替换数据。归档本身不加密，包含密码哈希、会话、邮件内容及加密后的商户/MFA 状态，必须另行加密保存到受控异地介质，并配置保留期、空间告警和独立访问权限。

证据统计会读取业务表；大量数据的排序/指纹会增加源库负载，长事务也影响清理。先在目标规模上测量耗时，再安排频率。默认单个数据库命令超时 600 秒，可用 `--timeout` 在 5 至 7200 秒间设置；超时失败不能当作成功备份。工具不包含全局角色、数据库级运维配置或持续 WAL 归档，恢复点只能到最近成功快照。

## 密钥、Agent 与恢复后的安全状态

数据库归档不能替代以下独立备份：

- 控制端私有环境配置、`MFA_KEY_FILE` 指向的原始加密密钥、支付签名/解密密钥、SMTP 凭据、TLS 证书与私钥。密钥与归档分开加密保管；没有原 MFA 密钥，已加密的因素不能解密，服务按失败关闭处理。数据库清单不包含这些密钥。
- 每个 Agent 的身份文件、节点证书、配置及私有 `usage/` 目录。先正常停止 Agent，确认退出，再整体备份 SQLite 主文件和仍存在的 WAL/SHM；不能在线只复制 `ledger.sqlite`。需要在线备份时应使用 SQLite 官方一致性备份流程，当前 PostgreSQL 工具不会操作 Agent 磁盘。
- 主机/容器部署、发布清单、兼容应用制品和上游代理配置。

恢复旧快照可能重新出现已经撤销的会话、重置链接和已用恢复码，也可能重新出现待发送邮件/待执行操作。切换前应在受控恢复库全量注销会话、使旧账号令牌和待发送邮件失效、删除旧 MFA 恢复码及待确认设置，并重新确认高风险队列。下面 SQL 仅用于**已核验、尚未开放流量的新恢复库**，不是对在线源库的日常操作；记录执行结果，之后再做账号和队列验收：

```sql
BEGIN;
DELETE FROM sessions;
UPDATE users SET security_version = security_version + 1;
UPDATE account_tokens SET consumed_at = now() WHERE consumed_at IS NULL;
UPDATE mail_outbox SET cancelled_at = now()
 WHERE sent_at IS NULL AND cancelled_at IS NULL;
DELETE FROM staff_mfa_recovery;
UPDATE staff_mfa SET pending_cipher = NULL, pending_expires_at = NULL,
 pending_version = NULL, pending_session_hash = NULL;
COMMIT;
```

已启用 TOTP 的员工仍需原验证器；恢复码需经过重新设置因素后重新生成。恢复工具不会自动执行上述业务变更，原始还原证据在变更前完成比较。支付/退款队列必须结合渠道现状逐项确认，避免把备份中的旧状态当作渠道当前状态。

同一节点身份和 SQLite 账本只能运行一个实例，不能同时启动恢复副本与旧节点。控制端去重可防止仍存在的旧批次重复入账，但控制库备份之后已确认并从 Agent 删除的记录未必能从 Agent 找回。需要更小 RPO 时另行建设并验收 PostgreSQL WAL 持续归档/PITR；本轮不承诺任意主机故障零丢数。恢复时间也以目标规模演练测量，不用小样本代替生产 RTO。

## 本地可重复验收

```sh
python3 -m unittest discover -s scripts -p test_backup.py -v
python3 scripts/backup.py self-test --directory .local/backups
bash scripts/go.sh test -race -count=1 ./internal/usageledger -run TestSIGKILL
```

`self-test` 仅创建随机独立合成库，按当前真实迁移列表建表，写入合成支付、订单、权益、用量和 MFA 状态，执行备份、恢复与证据核对，删除自己创建的源/目标库，保留私有合成归档供复查。不会备份或覆盖真实业务库。SQLite 测试使用真实子进程 `SIGKILL`：分别验证已提交 WAL 可恢复、未提交游标/批次不可见、序号和累计差值继续正确。它验证事务边界，不覆盖硬件失效或尚未采样的内核流量窗口。
