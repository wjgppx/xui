ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK(status IN ('pending','expired','closing','closed','paid','refunded'));
ALTER TABLE payment_activity DROP CONSTRAINT payment_activity_source_check;
ALTER TABLE payment_activity ADD CONSTRAINT payment_activity_source_check CHECK(source IN ('create','notify','query','auto_query','close','retry','refund','gateway_close'));
ALTER TABLE payment_activity DROP CONSTRAINT payment_activity_outcome_check;
ALTER TABLE payment_activity ADD CONSTRAINT payment_activity_outcome_check CHECK(outcome IN ('created','ready','paid','pending','not_paid','gateway_error','verification_error','storage_error','conflict','provider_unavailable','closed','retry_scheduled','manual_required','requested','approved','submitted','succeeded','failed','not_found','unsupported'));
CREATE TABLE payment_refunds (
 id text PRIMARY KEY, payment_id text NOT NULL UNIQUE REFERENCES payment_intents(id),
 order_id text NOT NULL REFERENCES orders(id), provider text NOT NULL CHECK(provider IN ('alipay','wechat')),
 transaction_id text NOT NULL, amount_minor bigint NOT NULL CHECK(amount_minor>0), currency text NOT NULL CHECK(currency='CNY'),
 reason text NOT NULL CHECK(octet_length(reason) BETWEEN 1 AND 256),
 status text NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','queued','processing','succeeded','failed','manual_required')),
 requested_by text NOT NULL REFERENCES users(id), approved_by text REFERENCES users(id),
 provider_refund_id text,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),completed_at timestamptz,
 CHECK((status='succeeded')=(completed_at IS NOT NULL)),
 CHECK(provider_refund_id IS NULL OR length(provider_refund_id) BETWEEN 1 AND 128)
);
CREATE TABLE payment_action_jobs (
 id text PRIMARY KEY, kind text NOT NULL CHECK(kind IN ('close','refund')),
 payment_id text NOT NULL REFERENCES payment_intents(id), refund_id text REFERENCES payment_refunds(id),
 reason text NOT NULL,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','processing','waiting','done','manual_required','failed')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0), checks integer NOT NULL DEFAULT 0 CHECK(checks>=0),
 submitted boolean NOT NULL DEFAULT false,
 last_outcome text NOT NULL DEFAULT '',next_attempt_at timestamptz NOT NULL DEFAULT now(),horizon_at timestamptz NOT NULL DEFAULT (now()+interval '7 days'),
 claim_token text,lease_until timestamptz,created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(payment_id,kind), UNIQUE(refund_id),
 CHECK((kind='refund')=(refund_id IS NOT NULL)),
 CHECK((claim_token IS NULL)=(lease_until IS NULL)),CHECK((status='processing')=(claim_token IS NOT NULL))
);
CREATE INDEX payment_actions_due ON payment_action_jobs(next_attempt_at) WHERE status IN ('queued','waiting','processing');
