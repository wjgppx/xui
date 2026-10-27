package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func TestConcurrentPaymentIsAppliedOnce(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	u := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, u, "explore")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Settle(ctx, "test", "event-1", o.ID, u, o.Price, o.Currency) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"payment_events", "entitlement_periods", "outbox_events"} {
		var n int
		if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
	orders, err := s.Orders(ctx, u)
	if err != nil || orders[0].Status != "paid" {
		t.Fatalf("order not paid: %v", err)
	}
}
func TestWrongAmountAndCurrencyCannotGrant(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	u := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, u, "explore")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		amount   int64
		currency string
	}{{1, "CNY"}, {o.Price, "USD"}} {
		if err = s.Settle(ctx, "test", store.ID(), o.ID, u, tc.amount, tc.currency); !errors.Is(err, store.ErrPaymentConflict) {
			t.Fatalf("expected conflict, got %v", err)
		}
	}
	e, err := s.Entitlements(ctx, u)
	if err != nil || len(e) != 0 {
		t.Fatal("invalid payment granted entitlement")
	}
}
func TestOrderSnapshotSurvivesPlanChanges(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	u := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, u, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE plans SET price_minor=9999,quota_bytes=1,name='changed' WHERE id='explore'`); err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, "test", "snapshot", o.ID, u, o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	e, err := s.Entitlements(ctx, u)
	if err != nil || len(e) != 1 || e[0].Quota != o.Quota || e[0].PlanName != o.PlanName {
		t.Fatalf("snapshot changed: %+v %v", e, err)
	}
}
func TestConcurrentRenewalsDoNotOverlap(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	u := testutil.User(t, s)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		o, err := s.CreateOrder(ctx, u, "everyday")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Settle(ctx, "test", o.ID, o.ID, u, o.Price, o.Currency) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	periods, err := s.Entitlements(ctx, u)
	if err != nil || len(periods) != 4 {
		t.Fatalf("periods=%v err=%v", periods, err)
	}
	for i := 1; i < len(periods); i++ {
		if !periods[i].StartsAt.Equal(periods[i-1].EndsAt) {
			t.Fatal("renewal periods overlap or have a gap")
		}
	}
}
func TestPaymentEventReuseRejected(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		u := testutil.User(t, s)
		o, err := s.CreateOrder(ctx, u, "explore")
		if err != nil {
			t.Fatal(err)
		}
		err = s.Settle(ctx, "test", "same-event", o.ID, u, o.Price, o.Currency)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !errors.Is(err, store.ErrPaymentConflict) {
			t.Fatalf("expected conflict: %v", err)
		}
	}
}
func TestOutboxFailureRollsBackPaymentAndEntitlement(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	u := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, u, "explore")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(ctx, `CREATE FUNCTION reject_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END $$; CREATE TRIGGER reject_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_outbox()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, "test", "rollback", o.ID, u, o.Price, o.Currency); err == nil {
		t.Fatal("injected transaction should fail")
	}
	for _, table := range []string{"payment_events", "entitlement_periods", "outbox_events"} {
		var count int
		if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial write in %s: %d %v", table, count, err)
		}
	}
	orders, err := s.Orders(ctx, u)
	if err != nil || orders[0].Status != "pending" {
		t.Fatal("order changed despite rollback")
	}
}
func TestMigrationAndSeedRepeatable(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.Plans(ctx)
	if err != nil || len(p) != 3 {
		t.Fatal("seed not repeatable")
	}
}
