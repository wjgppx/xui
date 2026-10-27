package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func enqueue(t *testing.T, s *store.Store, kind, aggregate string, payload any) string {
	t.Helper()
	id := store.ID()
	data, _ := json.Marshal(payload)
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO outbox_events(id,kind,aggregate_id,payload) VALUES($1,$2,$3,$4)`, id, kind, aggregate, data); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestOutboxConcurrentWorkersAndReplayDeliverOnce(t *testing.T) {
	ctx := context.Background()
	s := testutil.Database(t)
	node := storeNode(t, s)
	event := enqueue(t, s, "node_group.changed", store.ID(), map[string]any{"node_ids": []string{node}})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.ProcessOutbox(ctx, 10); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var revision int64
	var deliveries int
	s.DB.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id=$1`, node).Scan(&revision)
	s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox_deliveries WHERE event_id=$1`, event).Scan(&deliveries)
	if revision != 2 || deliveries != 1 {
		t.Fatal("event delivered more than once", revision, deliveries)
	}
	// Model an operator replay of an uncertain result: delivery evidence remains.
	if _, err := s.DB.Exec(ctx, `UPDATE outbox_events SET processed_at=NULL WHERE id=$1`, event); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessOutbox(ctx, 1); err != nil {
		t.Fatal(err)
	}
	s.DB.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id=$1`, node).Scan(&revision)
	if revision != 2 {
		t.Fatal("replay re-applied node invalidation")
	}
}
func TestOutboxEffectsRollbackRetryAndPoisonIsolation(t *testing.T) {
	ctx := context.Background()
	s := testutil.Database(t)
	node := storeNode(t, s)
	event := enqueue(t, s, "node_group.changed", store.ID(), map[string]any{"node_ids": []string{node}})
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_node_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected node update failure'; END $$; CREATE TRIGGER reject_node_update BEFORE UPDATE ON nodes FOR EACH ROW EXECUTE FUNCTION reject_node_update()`); err != nil {
		t.Fatal(err)
	}
	if count, err := s.ProcessOutbox(ctx, 5); err != nil || count != 0 {
		t.Fatal("failed event reported successful", count, err)
	}
	var deliveries, attempts int
	var processed *time.Time
	s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox_deliveries`).Scan(&deliveries)
	s.DB.QueryRow(ctx, `SELECT attempts,processed_at FROM outbox_events WHERE id=$1`, event).Scan(&attempts, &processed)
	if deliveries != 0 || attempts != 1 || processed != nil {
		t.Fatal("partial effect survived transaction failure")
	}
	if _, err := s.DB.Exec(ctx, `DROP TRIGGER reject_node_update ON nodes`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE outbox_events SET available_at=now() WHERE id=$1`, event); err != nil {
		t.Fatal(err)
	}
	if count, err := s.ProcessOutbox(ctx, 5); err != nil || count != 1 {
		t.Fatal("retry did not deliver", count, err)
	}
	poison := enqueue(t, s, "unknown.kind", store.ID(), map[string]any{})
	good := enqueue(t, s, "node_group.changed", store.ID(), map[string]any{"node_ids": []string{node}})
	if count, err := s.ProcessOutbox(ctx, 10); err != nil || count != 1 {
		t.Fatal("poison blocked peer", count, err)
	}
	var goodProcessed *time.Time
	s.DB.QueryRow(ctx, `SELECT processed_at FROM outbox_events WHERE id=$1`, good).Scan(&goodProcessed)
	if goodProcessed == nil {
		t.Fatal("healthy event not committed")
	}
	for i := 0; i < 4; i++ {
		s.DB.Exec(ctx, `UPDATE outbox_events SET available_at=now() WHERE id=$1`, poison)
		if _, err := s.ProcessOutbox(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	var dead *time.Time
	var code string
	s.DB.QueryRow(ctx, `SELECT attempts,dead_lettered_at,last_error FROM outbox_events WHERE id=$1`, poison).Scan(&attempts, &dead, &code)
	if attempts != 5 || dead == nil || code != "unsupported_kind" {
		t.Fatal("poison event not dead-lettered", attempts, code)
	}
	if count, err := s.ProcessOutbox(ctx, 10); err != nil || count != 0 {
		t.Fatal("dead-lettered event retried automatically")
	}
}
func TestScheduledEntitlementTransitionsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	s := testutil.Database(t)
	user := testutil.User(t, s)
	node := storeNode(t, s)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.EnsureNodeGrant(ctx, tx, node, user); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := storePaid(t, s, user, "explore")
	second := storePaid(t, s, user, "explore")
	if err = s.ScheduleEntitlementEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleEntitlementEvents(ctx); err != nil {
		t.Fatal(err)
	}
	var active int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE kind='entitlement.activated'`).Scan(&active)
	if active != 1 {
		t.Fatal("queued renewal activated early or duplicate transition", active)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE entitlement_periods SET starts_at=now()-interval '2 seconds',ends_at=now()-interval '1 second' WHERE order_id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE entitlement_periods SET starts_at=now()-interval '1 second' WHERE order_id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleEntitlementEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleEntitlementEvents(ctx); err != nil {
		t.Fatal(err)
	}
	var expired int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE kind='entitlement.activated'`).Scan(&active)
	s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE kind='entitlement.expired'`).Scan(&expired)
	if active != 2 || expired != 1 {
		t.Fatal("wall-time transitions missing or duplicated", active, expired)
	}
	if count, err := s.ProcessOutbox(ctx, 50); err != nil || count != 5 {
		t.Fatal("transition consumer failed", count, err)
	}
}
