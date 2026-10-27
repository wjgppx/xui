ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK(status IN ('pending','expired','closed','paid'));
ALTER TABLE orders ADD COLUMN expires_at timestamptz;
UPDATE orders SET expires_at=created_at+interval '30 minutes';
ALTER TABLE orders ALTER COLUMN expires_at SET DEFAULT (now()+interval '30 minutes');
ALTER TABLE orders ALTER COLUMN expires_at SET NOT NULL;
ALTER TABLE orders ADD COLUMN closed_at timestamptz;
ALTER TABLE orders ADD CONSTRAINT orders_closed_time CHECK((status='closed')=(closed_at IS NOT NULL));
CREATE INDEX orders_pending_expiry ON orders(expires_at) WHERE status='pending';

CREATE TABLE payment_reconciliation (
 payment_id text PRIMARY KEY REFERENCES payment_intents(id),
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','processing','waiting','paid','manual_required')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 checks integer NOT NULL DEFAULT 0 CHECK(checks>=0),
 last_outcome text NOT NULL DEFAULT '',
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 claim_token text, lease_until timestamptz,
 horizon_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((claim_token IS NULL)=(lease_until IS NULL)),
 CHECK((status='processing')=(claim_token IS NOT NULL))
);
CREATE INDEX payment_reconciliation_due ON payment_reconciliation(next_attempt_at)
 WHERE status IN ('queued','waiting','processing');

CREATE TABLE payment_activity (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 payment_id text REFERENCES payment_intents(id), order_id text REFERENCES orders(id),
 provider text NOT NULL CHECK(provider IN ('','alipay','wechat','epay_alipay','epay_wechat')),
 source text NOT NULL CHECK(source IN ('create','notify','query','auto_query','close','retry')),
 outcome text NOT NULL CHECK(outcome IN ('created','ready','paid','pending','not_paid','gateway_error','verification_error','storage_error','conflict','provider_unavailable','closed','retry_scheduled','manual_required')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX payment_activity_payment ON payment_activity(payment_id,id DESC);
CREATE FUNCTION payment_activity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'payment activity is append-only';
END $$;
CREATE TRIGGER payment_activity_no_update BEFORE UPDATE OR DELETE ON payment_activity
 FOR EACH ROW EXECUTE FUNCTION payment_activity_immutable();
