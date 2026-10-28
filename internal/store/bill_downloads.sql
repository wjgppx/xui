CREATE TABLE bill_download_jobs (
 id text PRIMARY KEY,
 provider text NOT NULL CHECK(provider IN ('alipay','wechat')),
 kind text NOT NULL CHECK(kind IN ('payment','refund')),
 business_date date NOT NULL,
 merchant_fingerprint text NOT NULL CHECK(merchant_fingerprint ~ '^[0-9a-f]{64}$'),
 requested_by text NOT NULL REFERENCES users(id),
 actor_version bigint NOT NULL CHECK(actor_version>=0),
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','processing','waiting','succeeded','manual_required')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 last_outcome text NOT NULL DEFAULT 'requested' CHECK(last_outcome IN ('requested','retry_scheduled','succeeded','gateway_error','verification_error','parse_error','unsupported','provider_unavailable','merchant_changed','actor_changed','not_ready','manual_required','storage_error')),
 statement_id text REFERENCES merchant_statements(id),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 horizon_at timestamptz NOT NULL DEFAULT (clock_timestamp()+interval '7 days'),
 claim_token text,lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),completed_at timestamptz,
 UNIQUE(provider,kind,business_date,merchant_fingerprint),
 CHECK((claim_token IS NULL)=(lease_until IS NULL)),
 CHECK((status='processing')=(claim_token IS NOT NULL)),
 CHECK((status='succeeded')=(statement_id IS NOT NULL)),
 CHECK((status='succeeded')=(completed_at IS NOT NULL))
);
CREATE INDEX bill_download_due ON bill_download_jobs(next_attempt_at) WHERE status IN ('queued','waiting','processing');
CREATE TABLE merchant_statement_sources (
 id text PRIMARY KEY,
 statement_id text NOT NULL REFERENCES merchant_statements(id),
 source_format text NOT NULL CHECK(source_format IN ('normalized_csv_v1','alipay_trade_v1','wechat_success_v1','wechat_refund_v1')),
 source_kind text NOT NULL CHECK(source_kind IN ('upload','download')),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 integrity text NOT NULL CHECK(integrity IN ('unverified_upload','wechat_signed_sha1','alipay_signed_url')),
 merchant_fingerprint text NOT NULL DEFAULT '' CHECK(merchant_fingerprint='' OR merchant_fingerprint ~ '^[0-9a-f]{64}$'),
 download_job_id text REFERENCES bill_download_jobs(id),
 imported_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(statement_id,source_format,source_kind,digest,merchant_fingerprint),
 CHECK((source_kind='upload')=(download_job_id IS NULL)),
 CHECK((source_kind='upload')=(integrity='unverified_upload'))
);
CREATE INDEX merchant_statement_sources_statement ON merchant_statement_sources(statement_id,created_at DESC,id);
INSERT INTO merchant_statement_sources(id,statement_id,source_format,source_kind,digest,integrity,imported_by)
 SELECT id,id,'normalized_csv_v1','upload',digest,'unverified_upload',imported_by FROM merchant_statements;
CREATE TRIGGER merchant_statement_sources_no_update BEFORE UPDATE OR DELETE ON merchant_statement_sources
 FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
