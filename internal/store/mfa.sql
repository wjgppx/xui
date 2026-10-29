ALTER TABLE sessions ADD COLUMN mfa_verified_at timestamptz;
CREATE TABLE staff_mfa (
 user_id text PRIMARY KEY REFERENCES users(id),
 secret_cipher bytea,
 enabled_at timestamptz,
 last_counter bigint NOT NULL DEFAULT -1 CHECK(last_counter>=-1),
 pending_cipher bytea,
 pending_expires_at timestamptz,
 pending_version bigint,
 pending_session_hash text,
 CHECK((secret_cipher IS NULL)=(enabled_at IS NULL)),
 CHECK((pending_cipher IS NULL)=(pending_expires_at IS NULL)),
 CHECK((pending_cipher IS NULL)=(pending_version IS NULL)),
 CHECK((pending_cipher IS NULL)=(pending_session_hash IS NULL))
);
CREATE TABLE staff_mfa_recovery (
 user_id text NOT NULL REFERENCES staff_mfa(user_id),
 code_hash text NOT NULL CHECK(code_hash ~ '^[0-9a-f]{64}$'),
 used_at timestamptz,
 PRIMARY KEY(user_id,code_hash)
);
