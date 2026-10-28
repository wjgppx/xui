CREATE TABLE bill_download_schedules (
 id text PRIMARY KEY,
 provider text NOT NULL CHECK(provider IN ('alipay','wechat')),
 kind text NOT NULL CHECK(kind IN ('payment','refund')),
 enabled boolean NOT NULL DEFAULT false,
 run_minute integer NOT NULL DEFAULT 600 CHECK(run_minute BETWEEN 0 AND 1439),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>=1),
 state text NOT NULL DEFAULT 'disabled' CHECK(state IN ('disabled','active','actor_changed','merchant_changed','provider_unavailable')),
 next_bill_date date,
 last_enqueued_date date,
 last_job_id text REFERENCES bill_download_jobs(id),
 authorized_by text REFERENCES users(id),
 actor_version bigint CHECK(actor_version>=0),
 merchant_fingerprint text NOT NULL DEFAULT '' CHECK(merchant_fingerprint='' OR merchant_fingerprint ~ '^[0-9a-f]{64}$'),
 authorization_generation bigint NOT NULL DEFAULT 1 CHECK(authorization_generation>=1),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(provider,kind),
 CHECK(enabled=(state='active')),
 CHECK(NOT enabled OR (next_bill_date IS NOT NULL AND authorized_by IS NOT NULL AND actor_version IS NOT NULL AND merchant_fingerprint<>''))
);
INSERT INTO bill_download_schedules(id,provider,kind) VALUES
 ('bill-alipay-payment','alipay','payment'),('bill-alipay-refund','alipay','refund'),
 ('bill-wechat-payment','wechat','payment'),('bill-wechat-refund','wechat','refund');
ALTER TABLE bill_download_jobs ADD COLUMN schedule_id text REFERENCES bill_download_schedules(id);
ALTER TABLE bill_download_jobs ADD COLUMN schedule_generation bigint CHECK(schedule_generation>=1);
ALTER TABLE bill_download_jobs ADD CONSTRAINT bill_download_schedule_pair CHECK((schedule_id IS NULL)=(schedule_generation IS NULL));
ALTER TABLE bill_download_jobs DROP CONSTRAINT bill_download_jobs_last_outcome_check;
ALTER TABLE bill_download_jobs ADD CONSTRAINT bill_download_jobs_last_outcome_check CHECK(last_outcome IN ('requested','retry_scheduled','succeeded','gateway_error','verification_error','parse_error','unsupported','provider_unavailable','merchant_changed','actor_changed','schedule_changed','not_ready','manual_required','storage_error'));
