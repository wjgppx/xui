CREATE TABLE subscriptions (
 user_id text PRIMARY KEY REFERENCES users(id),
 token_hash text UNIQUE,
 created_at timestamptz NOT NULL DEFAULT now()
);
