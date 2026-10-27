package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func storeNode(t *testing.T, s *store.Store) string {
	t.Helper()
	id := store.ID()
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO nodes(id,name,enrollment_hash,enrollment_expires_at) VALUES($1,'store test',$2,now()+interval '15 minutes')`, id, store.ID()); err != nil {
		t.Fatal(err)
	}
	return id
}
func storeGroup(t *testing.T, s *store.Store, nodes ...string) string {
	t.Helper()
	id := store.ID()
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO node_groups(id,name) VALUES($1,'group')`, id); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if _, err := s.DB.Exec(context.Background(), `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2)`, id, node); err != nil {
			t.Fatal(err)
		}
	}
	return id
}
func storePaid(t *testing.T, s *store.Store, user, plan string) store.Order {
	t.Helper()
	order, err := s.CreateOrder(context.Background(), user, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(context.Background(), "test", store.ID(), order.ID, user, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	return order
}
func subjects(t *testing.T, s *store.Store, node, mode string, emails []string) []store.AuthorizedSubject {
	t.Helper()
	tx, err := s.DB.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	out, err := s.BindingSubjects(context.Background(), tx, node, mode, emails, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestPurchasedGroupsSnapshotSurvivesCatalogChangeAndQueuedRenewal(t *testing.T) {
	ctx := context.Background()
	s := testutil.Database(t)
	user := testutil.User(t, s)
	nodeA, nodeB := storeNode(t, s), storeNode(t, s)
	a, b := storeGroup(t, s, nodeA), storeGroup(t, s, nodeB)
	if _, err := s.DB.Exec(ctx, `INSERT INTO plan_node_groups(plan_id,group_id) VALUES('explore',$1)`, a); err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `DELETE FROM plan_node_groups WHERE plan_id='explore'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO plan_node_groups(plan_id,group_id) VALUES('explore',$1)`, b); err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, "test", "snapshot", first.ID, user, first.Price, first.Currency); err != nil {
		t.Fatal(err)
	}
	second := storePaid(t, s, user, "explore")
	if got := subjects(t, s, nodeA, "plan", nil); len(got) != 1 || got[0].UserID != user {
		t.Fatal("first order lost purchased group snapshot")
	}
	if got := subjects(t, s, nodeB, "plan", nil); len(got) != 0 {
		t.Fatal("future renewal authorized before its start")
	}
	var firstGroup, secondGroup string
	if err = s.DB.QueryRow(ctx, `SELECT eg.group_id FROM entitlement_node_groups eg JOIN entitlement_periods e ON e.id=eg.entitlement_period_id WHERE e.order_id=$1`, first.ID).Scan(&firstGroup); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT eg.group_id FROM entitlement_node_groups eg JOIN entitlement_periods e ON e.id=eg.entitlement_period_id WHERE e.order_id=$1`, second.ID).Scan(&secondGroup); err != nil {
		t.Fatal(err)
	}
	if firstGroup != a || secondGroup != b {
		t.Fatal("group snapshot copied from mutable catalog instead of order")
	}
	if _, err = s.DB.Exec(ctx, `UPDATE entitlement_periods SET starts_at=now()-interval '2 seconds',ends_at=now()-interval '1 second' WHERE order_id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE entitlement_periods SET starts_at=now()-interval '1 second' WHERE order_id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if len(subjects(t, s, nodeA, "plan", nil)) != 0 || len(subjects(t, s, nodeB, "plan", nil)) != 1 {
		t.Fatal("period transition did not switch purchased group access")
	}
}
func TestGroupDisableMembershipRevocationAndManualCompatibility(t *testing.T) {
	ctx := context.Background()
	s := testutil.Database(t)
	user := testutil.User(t, s)
	node, other := storeNode(t, s), storeNode(t, s)
	group := storeGroup(t, s, node)
	if _, err := s.DB.Exec(ctx, `INSERT INTO plan_node_groups(plan_id,group_id) VALUES('explore',$1)`, group); err != nil {
		t.Fatal(err)
	}
	order := storePaid(t, s, user, "explore")
	if len(subjects(t, s, node, "plan", nil)) != 1 {
		t.Fatal("plan user not authorized")
	}
	if len(subjects(t, s, other, "plan", nil)) != 0 {
		t.Fatal("other node leaked group access")
	}
	if len(subjects(t, s, other, "manual", []string{user + "@example.test"})) != 1 {
		t.Fatal("manual compatibility lost")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE node_groups SET enabled=false WHERE id=$1`, group); err != nil {
		t.Fatal(err)
	}
	if len(subjects(t, s, node, "plan", nil)) != 0 {
		t.Fatal("disabled group authorized")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE node_groups SET enabled=true WHERE id=$1`, group); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM node_group_members WHERE group_id=$1`, group); err != nil {
		t.Fatal(err)
	}
	if len(subjects(t, s, node, "plan", nil)) != 0 {
		t.Fatal("removed member authorized")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE entitlement_periods SET revoked_at=now() WHERE order_id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if len(subjects(t, s, other, "manual", []string{user + "@example.test"})) != 0 {
		t.Fatal("revoked manual entitlement authorized")
	}
}
func TestNodeGrantCreationIsStable(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	node, user := storeNode(t, s), testutil.User(t, s)
	var uuid, password string
	for i := 0; i < 2; i++ {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a, b, err := s.EnsureNodeGrant(ctx, tx, node, user)
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			uuid, password = a, b
		} else if uuid != a || password != b {
			t.Fatal("grant lookup changed persistent credentials")
		}
	}
	if len(uuid) != 36 || len(password) != 64 {
		t.Fatal("invalid generated protocol credential")
	}
}
