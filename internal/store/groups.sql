CREATE TABLE node_groups (
 id text PRIMARY KEY, name text NOT NULL, enabled boolean NOT NULL DEFAULT true,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE node_group_members (
 group_id text NOT NULL REFERENCES node_groups(id), node_id text NOT NULL REFERENCES nodes(id),
 PRIMARY KEY(group_id,node_id)
);
CREATE INDEX node_group_members_node ON node_group_members(node_id,group_id);
ALTER TABLE plans ADD COLUMN access_revision bigint NOT NULL DEFAULT 1 CHECK(access_revision>0);
CREATE TABLE plan_node_groups (
 plan_id text NOT NULL REFERENCES plans(id), group_id text NOT NULL REFERENCES node_groups(id),
 PRIMARY KEY(plan_id,group_id)
);
CREATE TABLE order_node_groups (
 order_id text NOT NULL REFERENCES orders(id), group_id text NOT NULL REFERENCES node_groups(id),
 PRIMARY KEY(order_id,group_id)
);
CREATE TABLE entitlement_node_groups (
 entitlement_period_id text NOT NULL REFERENCES entitlement_periods(id), group_id text NOT NULL REFERENCES node_groups(id),
 PRIMARY KEY(entitlement_period_id,group_id)
);
ALTER TABLE entitlement_periods ADD COLUMN revoked_at timestamptz;
ALTER TABLE outbox_events
 ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 ADD COLUMN last_error text NOT NULL DEFAULT '',
 ADD COLUMN available_at timestamptz NOT NULL DEFAULT now(),
 ADD COLUMN dead_lettered_at timestamptz;
CREATE INDEX outbox_pending ON outbox_events(available_at,created_at) WHERE processed_at IS NULL AND dead_lettered_at IS NULL;
CREATE TABLE outbox_deliveries (
 event_id text NOT NULL REFERENCES outbox_events(id), node_id text NOT NULL REFERENCES nodes(id),
 delivered_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(event_id,node_id)
);
