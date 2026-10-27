ALTER TABLE nodes
 ADD COLUMN configuration jsonb NOT NULL DEFAULT '{"bindings":[]}',
 ADD COLUMN desired_digest text NOT NULL DEFAULT '',
 ADD COLUMN applied_revision bigint NOT NULL DEFAULT 0 CHECK(applied_revision>=0),
 ADD COLUMN apply_status text NOT NULL DEFAULT 'pending' CHECK(apply_status IN ('pending','applied','failed')),
 ADD COLUMN apply_error text NOT NULL DEFAULT '',
 ADD COLUMN core_instance_id text NOT NULL DEFAULT '',
 ADD COLUMN applied_reported_at timestamptz;
CREATE TABLE node_grants (
 node_id text NOT NULL REFERENCES nodes(id), user_id text NOT NULL REFERENCES users(id),
 credential_id text NOT NULL, secret text NOT NULL,
 PRIMARY KEY(node_id,user_id)
);
