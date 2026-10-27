CREATE TABLE nodes (
 id text PRIMARY KEY, name text NOT NULL,
 enrollment_hash text NOT NULL UNIQUE, enrollment_expires_at timestamptz NOT NULL,
 agent_token_hash text UNIQUE, enrolled_at timestamptz, revoked_at timestamptz,
 desired_revision bigint NOT NULL DEFAULT 1 CHECK(desired_revision > 0),
 observed_revision bigint NOT NULL DEFAULT 0 CHECK(observed_revision >= 0),
 agent_version text NOT NULL DEFAULT '', last_seen_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
