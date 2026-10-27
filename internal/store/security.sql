ALTER TABLE users
 ADD COLUMN email_verified_at timestamptz,
 ADD COLUMN security_version bigint NOT NULL DEFAULT 0 CHECK(security_version>=0);
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK(role IN ('user','admin','support','finance','operator'));
ALTER TABLE sessions ADD COLUMN security_version bigint NOT NULL DEFAULT 0 CHECK(security_version>=0);

CREATE TABLE account_tokens (
 token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id),
 purpose text NOT NULL CHECK(purpose IN ('verify','reset')),
 expires_at timestamptz NOT NULL, consumed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(expires_at>created_at)
);
CREATE INDEX account_tokens_user_purpose ON account_tokens(user_id,purpose);
CREATE TABLE mail_outbox (
 id text PRIMARY KEY, to_email text NOT NULL, subject text NOT NULL, body text NOT NULL,
 token_hash text NOT NULL REFERENCES account_tokens(token_hash),
 created_at timestamptz NOT NULL DEFAULT now(), sent_at timestamptz, cancelled_at timestamptz,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0), last_error text NOT NULL DEFAULT '',
 next_attempt_at timestamptz NOT NULL DEFAULT now(), dead_lettered_at timestamptz
);
CREATE INDEX mail_outbox_ready ON mail_outbox(next_attempt_at,created_at)
 WHERE sent_at IS NULL AND cancelled_at IS NULL AND dead_lettered_at IS NULL;
CREATE TABLE rate_limit_buckets (
 key_hash text PRIMARY KEY, count integer NOT NULL CHECK(count>0), expires_at timestamptz NOT NULL
);
CREATE INDEX rate_limit_expiry ON rate_limit_buckets(expires_at);
