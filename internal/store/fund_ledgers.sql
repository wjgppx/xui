CREATE TABLE fund_ledger_accounts (
 account_key text PRIMARY KEY CHECK(account_key ~ '^[a-z][a-z0-9_-]{0,63}$'),
 provider text NOT NULL CHECK(provider IN ('alipay','wechat','bank')),
 currency text NOT NULL DEFAULT 'CNY' CHECK(currency='CNY'),
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE fund_ledger_batches (
 id text PRIMARY KEY,
 account_key text NOT NULL REFERENCES fund_ledger_accounts(account_key),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 document jsonb NOT NULL CHECK(jsonb_typeof(document)='object' AND document->>'version'='fund-ledger-v1' AND jsonb_typeof(document->'rows')='array' AND jsonb_array_length(document->'rows')<=2000),
 imported_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(account_key,digest)
);
CREATE INDEX fund_ledger_batches_latest ON fund_ledger_batches(created_at DESC,id DESC);
CREATE TABLE fund_ledger_reconciliations (
 id text PRIMARY KEY,
 merchant_batch_id text NOT NULL REFERENCES fund_ledger_batches(id),
 bank_batch_id text NOT NULL REFERENCES fund_ledger_batches(id),
 version text NOT NULL CHECK(version='fund-transfer-comparison-v1'),
 report jsonb NOT NULL CHECK(jsonb_typeof(report)='object' AND jsonb_typeof(report->'rows')='array' AND jsonb_array_length(report->'rows')<=4000),
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(merchant_batch_id,bank_batch_id,version),
 CHECK(merchant_batch_id<>bank_batch_id)
);
CREATE INDEX fund_ledger_reconciliations_latest ON fund_ledger_reconciliations(created_at DESC,id DESC);
CREATE TRIGGER fund_ledger_accounts_no_update BEFORE UPDATE OR DELETE ON fund_ledger_accounts
 FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER fund_ledger_batches_no_update BEFORE UPDATE OR DELETE ON fund_ledger_batches
 FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER fund_ledger_reconciliations_no_update BEFORE UPDATE OR DELETE ON fund_ledger_reconciliations
 FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
