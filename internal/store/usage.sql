-- Keep continuous issued authorization intervals, including historical periods.
-- Overlapping renewals extend a row; a lapse creates a new row so gaps survive.
CREATE TABLE node_usage_authorizations (
 node_id text NOT NULL REFERENCES nodes(id), binding_id text NOT NULL,
 user_id text NOT NULL REFERENCES users(id),
 entitlement_period_id text NOT NULL REFERENCES entitlement_periods(id),
 authorized_from timestamptz NOT NULL, authorized_until timestamptz NOT NULL,
 CHECK(authorized_until>authorized_from),
 PRIMARY KEY(node_id,binding_id,user_id,entitlement_period_id,authorized_from)
);
CREATE INDEX node_usage_authorization_lookup ON node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_until);

CREATE TABLE usage_batches (
 node_id text NOT NULL REFERENCES nodes(id), batch_id text NOT NULL,
 core_instance_id text NOT NULL, sequence bigint NOT NULL CHECK(sequence>0),
 started_at timestamptz NOT NULL, ended_at timestamptz NOT NULL,
 digest text NOT NULL, payload jsonb NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 CHECK(ended_at>started_at),
 PRIMARY KEY(node_id,batch_id), UNIQUE(node_id,core_instance_id,sequence)
);
CREATE TABLE usage_items (
 node_id text NOT NULL, batch_id text NOT NULL, item_index integer NOT NULL CHECK(item_index>=0),
 binding_id text NOT NULL, user_id text NOT NULL REFERENCES users(id),
 entitlement_period_id text NOT NULL REFERENCES entitlement_periods(id),
 uplink_bytes bigint NOT NULL CHECK(uplink_bytes>=0),
 downlink_bytes bigint NOT NULL CHECK(downlink_bytes>=0),
 CHECK(uplink_bytes>0 OR downlink_bytes>0),
 FOREIGN KEY(node_id,batch_id) REFERENCES usage_batches(node_id,batch_id),
 PRIMARY KEY(node_id,batch_id,item_index)
);
CREATE INDEX usage_items_entitlement ON usage_items(entitlement_period_id);
