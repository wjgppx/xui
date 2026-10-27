CREATE TABLE payment_intents (
 id text PRIMARY KEY,
 order_id text NOT NULL UNIQUE REFERENCES orders(id),
 provider text NOT NULL CHECK(provider IN ('alipay','wechat','epay_alipay','epay_wechat')),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','ready','paid')),
 checkout_kind text NOT NULL DEFAULT '' CHECK(checkout_kind IN ('','redirect','qr')),
 checkout_url text NOT NULL DEFAULT '',
 transaction_id text,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 paid_at timestamptz,
 CHECK((checkout_kind='')=(checkout_url='')),
 CHECK((status='paid')=(transaction_id IS NOT NULL)),
 CHECK((status='paid')=(paid_at IS NOT NULL)),
 CHECK(transaction_id IS NULL OR length(transaction_id) BETWEEN 1 AND 128)
);
CREATE UNIQUE INDEX payment_intents_transaction ON payment_intents(provider,transaction_id)
 WHERE transaction_id IS NOT NULL;
