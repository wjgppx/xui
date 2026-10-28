CREATE INDEX payment_intents_statement_day ON payment_intents(provider,paid_at,id) WHERE status='paid';
CREATE INDEX payment_refunds_statement_day ON payment_refunds(provider,completed_at,id) WHERE status='succeeded';
CREATE TABLE merchant_statements (
 id text PRIMARY KEY,
 provider text NOT NULL CHECK(provider IN ('alipay','wechat','epay_alipay','epay_wechat')),
 kind text NOT NULL CHECK(kind IN ('payment','refund')),
 business_date date NOT NULL,
 coverage text NOT NULL CHECK(coverage IN ('partial','full')),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 imported_by text NOT NULL REFERENCES users(id),
 row_count integer NOT NULL CHECK(row_count BETWEEN 0 AND 2000),
 total_minor bigint NOT NULL CHECK(total_minor>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(provider,kind,business_date,coverage,digest),
 CHECK(kind='payment' OR provider IN ('alipay','wechat'))
);
CREATE TABLE merchant_statement_rows (
 statement_id text NOT NULL REFERENCES merchant_statements(id),
 row_number integer NOT NULL CHECK(row_number BETWEEN 1 AND 2000),
 payment_id text NOT NULL,
 transaction_id text NOT NULL,
 refund_id text NOT NULL,
 provider_refund_id text NOT NULL,
 amount_minor bigint NOT NULL CHECK(amount_minor>0),
 currency text NOT NULL CHECK(currency='CNY'),
 occurred_at timestamptz NOT NULL,
 PRIMARY KEY(statement_id,row_number)
);
CREATE TABLE merchant_statement_runs (
 id text PRIMARY KEY,
 statement_id text NOT NULL REFERENCES merchant_statements(id),
 revision integer NOT NULL CHECK(revision>0),
 matched_count integer NOT NULL CHECK(matched_count BETWEEN 0 AND 2000),
 difference_count integer NOT NULL CHECK(difference_count BETWEEN 0 AND 4000),
 local_candidate_count integer NOT NULL CHECK(local_candidate_count BETWEEN 0 AND 2000),
 as_of timestamptz NOT NULL,
 created_by text NOT NULL REFERENCES users(id),
 UNIQUE(statement_id,revision),
 CHECK(local_candidate_count<=difference_count)
);
CREATE TABLE merchant_statement_findings (
 id text PRIMARY KEY,
 run_id text NOT NULL REFERENCES merchant_statement_runs(id),
 row_number integer,
 payment_id text NOT NULL,
 refund_id text NOT NULL,
 codes text[] NOT NULL CHECK(cardinality(codes)>0 AND codes <@ ARRAY['missing_local','provider_mismatch','identity_mismatch','amount_mismatch','not_confirmed','local_only']::text[]),
 statement_transaction_id text NOT NULL,
 local_transaction_id text NOT NULL,
 statement_provider_refund_id text NOT NULL,
 local_provider_refund_id text NOT NULL,
 statement_amount_minor bigint,
 local_amount_minor bigint,
 local_provider text NOT NULL,
 local_status text NOT NULL,
 local_confirmed_at timestamptz,
 statement_occurred_at timestamptz,
 CHECK(row_number IS NULL OR row_number BETWEEN 1 AND 2000),
 CHECK(statement_amount_minor IS NULL OR statement_amount_minor>0),
 CHECK(local_amount_minor IS NULL OR local_amount_minor>0)
);
CREATE INDEX merchant_statement_findings_run ON merchant_statement_findings(run_id,row_number,payment_id,refund_id,id);
CREATE TABLE merchant_statement_reviews (
 id text PRIMARY KEY,
 finding_id text NOT NULL REFERENCES merchant_statement_findings(id),
 actor_id text NOT NULL REFERENCES users(id),
 note text NOT NULL CHECK(octet_length(note) BETWEEN 1 AND 1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(finding_id,actor_id,note)
);
CREATE INDEX merchant_statement_reviews_finding ON merchant_statement_reviews(finding_id,created_at DESC,id);
CREATE FUNCTION merchant_statement_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'merchant statement evidence and reports are append-only';
END $$;
CREATE TRIGGER merchant_statements_no_update BEFORE UPDATE OR DELETE ON merchant_statements FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER merchant_statement_rows_no_update BEFORE UPDATE OR DELETE ON merchant_statement_rows FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER merchant_statement_runs_no_update BEFORE UPDATE OR DELETE ON merchant_statement_runs FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER merchant_statement_findings_no_update BEFORE UPDATE OR DELETE ON merchant_statement_findings FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER merchant_statement_reviews_no_update BEFORE UPDATE OR DELETE ON merchant_statement_reviews FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
