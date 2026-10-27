CREATE TABLE users (
 id text PRIMARY KEY, email text NOT NULL UNIQUE, password_hash text NOT NULL,
 role text NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
 token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id),
 expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE plans (
 id text PRIMARY KEY, name text NOT NULL, description text NOT NULL,
 price_minor bigint NOT NULL CHECK (price_minor > 0), currency text NOT NULL CHECK (currency = 'CNY'),
 duration_days integer NOT NULL CHECK (duration_days BETWEEN 1 AND 366),
 quota_bytes bigint NOT NULL CHECK (quota_bytes > 0), active boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE orders (
 id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id), plan_id text NOT NULL REFERENCES plans(id),
 plan_name text NOT NULL, price_minor bigint NOT NULL, currency text NOT NULL,
 duration_days integer NOT NULL, quota_bytes bigint NOT NULL,
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','paid')),
 created_at timestamptz NOT NULL DEFAULT now(), paid_at timestamptz
);
CREATE INDEX orders_user ON orders(user_id,created_at DESC);
CREATE TABLE payment_events (
 provider text NOT NULL, event_id text NOT NULL, order_id text NOT NULL REFERENCES orders(id),
 amount_minor bigint NOT NULL, currency text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(provider,event_id), UNIQUE(provider,order_id)
);
CREATE TABLE entitlement_periods (
 id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id), order_id text NOT NULL UNIQUE REFERENCES orders(id),
 plan_name text NOT NULL, starts_at timestamptz NOT NULL, ends_at timestamptz NOT NULL,
 quota_bytes bigint NOT NULL, used_bytes bigint NOT NULL DEFAULT 0 CHECK(used_bytes >= 0),
 CHECK (ends_at > starts_at)
);
CREATE INDEX entitlement_user ON entitlement_periods(user_id,ends_at);
CREATE TABLE outbox_events (
 id text PRIMARY KEY, kind text NOT NULL, aggregate_id text NOT NULL,
 payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), processed_at timestamptz,
 UNIQUE(kind,aggregate_id)
);
CREATE TABLE audit_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, actor_id text NOT NULL REFERENCES users(id),
 action text NOT NULL, resource_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
