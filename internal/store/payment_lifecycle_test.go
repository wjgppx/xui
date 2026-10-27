package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func lifecycleDue(t *testing.T, s *store.Store, paymentID string) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(), `UPDATE payment_reconciliation SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, paymentID); err != nil {
		t.Fatal(err)
	}
}
func TestLifecycleExpiryStopsCheckoutButAcceptsVerifiedLatePayment(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, o, p := paymentFixture(t, s, "alipay")
	if o.ExpiresAt.Sub(o.CreatedAt) != 30*time.Minute || o.ClosedAt != nil {
		t.Fatal("order deadline missing")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE orders SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, o.ID); err != nil {
		t.Fatal(err)
	}
	listed, err := s.Orders(ctx, user)
	if err != nil || len(listed) != 1 || listed[0].Status != "expired" {
		t.Fatal("expired order displayed payable", err)
	}
	if _, err = s.CreatePayment(ctx, user, o.ID, "alipay"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("expired checkout reopened", err)
	}
	if err = s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = s.DB.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, o.ID).Scan(&status); err != nil || status != "expired" {
		t.Fatal("expiry not persisted", err)
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "late-verified-payment", o.Price, o.Currency); err != nil {
		t.Fatal("verified late payment lost", err)
	}
	got, err := s.PaymentOrder(ctx, user, o.ID)
	if err != nil || got.Status != "paid" {
		t.Fatal("late payment did not fulfill", err)
	}
}

func TestLifecycleCloseIsOwnedAndCannotCancelAnExternalPayment(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	other := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CloseOrder(ctx, other, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("crossowner close", err)
	}
	closed, err := s.CloseOrder(ctx, user, o.ID)
	if err != nil || closed.Status != "closed" || closed.ClosedAt == nil {
		t.Fatal("unpaid local order not closed", err)
	}
	if _, err = s.CloseOrder(ctx, user, o.ID); err != nil {
		t.Fatal("close not idempotent", err)
	}
	if _, err = s.CreatePayment(ctx, user, o.ID, "alipay"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("closed order payment created", err)
	}
	if err = s.Settle(ctx, "development", "closed-order", o.ID, user, o.Price, o.Currency); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("closed order fulfilled", err)
	}
	user2, o2, _ := paymentFixture(t, s, "epay_alipay")
	if _, err = s.CloseOrder(ctx, user2, o2.ID); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("local close pretended remote cancellation", err)
	}
	activity, err := s.RecentPaymentActivity(ctx)
	if err != nil || len(activity) != 1 || activity[0].OrderID == nil || *activity[0].OrderID != o.ID || activity[0].Source != "close" {
		t.Fatal("close audit missing", err)
	}
}

func TestLifecycleCloseAndCreationSerializeWithoutAmbiguousState(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	var createErr, closeErr error
	go func() { defer wg.Done(); _, createErr = s.CreatePayment(ctx, user, o.ID, "wechat") }()
	go func() { defer wg.Done(); _, closeErr = s.CloseOrder(ctx, user, o.ID) }()
	wg.Wait()
	if (createErr == nil) == (closeErr == nil) {
		t.Fatal("creation and close must have exactly one winner", createErr, closeErr)
	}
	loser := createErr
	if loser == nil {
		loser = closeErr
	}
	if !errors.Is(loser, store.ErrPaymentConflict) {
		t.Fatal(loser)
	}
	var invalid bool
	if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders o JOIN payment_intents p ON p.order_id=o.id WHERE o.status='closed')`).Scan(&invalid); err != nil || invalid {
		t.Fatal("closed order has an external payment", err)
	}
}

func TestLifecycleClaimsAreLeasedAndCrashRecoveryRejectsStaleCompletion(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	_, _, p := paymentFixture(t, s, "wechat")
	paymentFixture(t, s, "epay_alipay")
	if err := s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	lifecycleDue(t, s, p.ID)
	type claim struct {
		job store.PaymentJob
		err error
	}
	results := make(chan claim, 8)
	for i := 0; i < 8; i++ {
		go func() { j, err := s.ClaimPaymentReconciliation(ctx); results <- claim{j, err} }()
	}
	var first store.PaymentJob
	claimed := 0
	for i := 0; i < 8; i++ {
		r := <-results
		if r.err == nil {
			first = r.job
			claimed++
		} else if !errors.Is(r.err, store.ErrNotFound) {
			t.Fatal(r.err)
		}
	}
	if claimed != 1 || first.PaymentID != p.ID || first.Checks != 1 || first.ClaimToken == "" {
		t.Fatal("lease claim was not exclusive", claimed)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE payment_reconciliation SET lease_until=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimPaymentReconciliation(ctx)
	if err != nil || second.ClaimToken == first.ClaimToken || second.Checks != 2 {
		t.Fatal("crashed claim not recovered", err)
	}
	if err = s.FinishPaymentReconciliation(ctx, p.ID, first.ClaimToken, "pending"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("stale claim completed", err)
	}
	if err = s.FinishPaymentReconciliation(ctx, p.ID, second.ClaimToken, "pending"); err != nil {
		t.Fatal(err)
	}
	var count, attempts int
	if err = s.DB.QueryRow(ctx, `SELECT count(*),max(attempts) FROM payment_reconciliation`).Scan(&count, &attempts); err != nil || count != 1 || attempts != 0 {
		t.Fatal("Epay scheduled or pending consumed failure budget", err)
	}
}

func TestLifecycleFailuresBackoffAndExplicitRetryIsAudited(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	_, _, p := paymentFixture(t, s, "alipay")
	if err := s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"gateway_error", "pending"} {
		lifecycleDue(t, s, p.ID)
		j, err := s.ClaimPaymentReconciliation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishPaymentReconciliation(ctx, p.ID, j.ClaimToken, outcome); err != nil {
			t.Fatal(err)
		}
	}
	var consecutive int
	if err := s.DB.QueryRow(ctx, `SELECT attempts FROM payment_reconciliation WHERE payment_id=$1`, p.ID).Scan(&consecutive); err != nil || consecutive != 0 {
		t.Fatal("authenticated pending did not clear consecutive errors", err)
	}
	for i := 0; i < 8; i++ {
		lifecycleDue(t, s, p.ID)
		j, err := s.ClaimPaymentReconciliation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishPaymentReconciliation(ctx, p.ID, j.ClaimToken, "gateway_error"); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var attempts int
	if err := s.DB.QueryRow(ctx, `SELECT status,attempts FROM payment_reconciliation WHERE payment_id=$1`, p.ID).Scan(&state, &attempts); err != nil || state != "manual_required" || attempts != 8 {
		t.Fatal("failure budget not enforced", err, state, attempts)
	}
	admin := testutil.User(t, s)
	if err := s.RetryPaymentReconciliation(ctx, admin, p.ID); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("ordinary user retried payment", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET role='finance' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryPaymentReconciliation(ctx, admin, p.ID); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimPaymentReconciliation(ctx)
	if err != nil || j.Attempts != 0 {
		t.Fatal("explicit retry not due", err)
	}
	if err = s.RetryPaymentReconciliation(ctx, admin, p.ID); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("active lease stolen", err)
	}
	activities, err := s.PaymentActivity(ctx, p.ID)
	if err != nil || len(activities) != 11 || activities[0].Source != "retry" {
		t.Fatal("retry/activity audit missing", err, len(activities))
	}
	var audit int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_id=$1 AND action='payment.reconciliation_retried'`, admin).Scan(&audit); err != nil || audit != 1 {
		t.Fatal("staff retry audit missing", err)
	}
}

func TestLifecyclePendingHorizonStopsTrafficAndCallbackWins(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, o, p := paymentFixture(t, s, "wechat")
	if err := s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	lifecycleDue(t, s, p.ID)
	j, err := s.ClaimPaymentReconciliation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "callback-race", o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	if err = s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPaymentReconciliation(ctx, p.ID, j.ClaimToken, "gateway_error"); err != nil {
		t.Fatal("callback stole live lease", err)
	}
	list, err := s.ListPayments(ctx, "paid", "", o.ID)
	if err != nil || len(list) != 1 || list[0].ReconcileStatus != "paid" || list[0].LastOutcome != "paid" || list[0].NextCheckAt != nil {
		t.Fatal("callback race downgraded paid", err)
	}
	if _, err = s.Payment(ctx, user, o.ID); err != nil {
		t.Fatal(err)
	}
	_, oldOrder, old := paymentFixture(t, s, "alipay")
	if err = s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE payment_reconciliation SET horizon_at=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := s.PaymentActivity(ctx, old.ID)
	if err != nil || len(history) != 1 || history[0].Outcome != "manual_required" {
		t.Fatal("pending horizon not recorded", err)
	}
	if _, err = s.ClaimPaymentReconciliation(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("exhausted horizon queried again", err)
	}
	// The verified callback can arrive before the scheduler has converged the
	// review job. A paid transaction must leave the actionable review filter now.
	if err = s.SettlePayment(ctx, old.ID, old.Provider, "late-review-payment", oldOrder.Price, oldOrder.Currency); err != nil {
		t.Fatal(err)
	}
	review, err := s.ListPayments(ctx, "manual_required", "", oldOrder.ID)
	if err != nil || len(review) != 0 {
		t.Fatal("paid transaction remained in manual review", err)
	}

}

func TestLifecycleActivityBindsOnlyKnownMatchingMerchantAndIsAppendOnly(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	_, o, p := paymentFixture(t, s, "alipay")
	for _, v := range []struct{ id, provider, outcome string }{{p.ID, "alipay", "ready"}, {p.ID, "wechat", "conflict"}, {"unknown", "alipay", "conflict"}, {"", "epay_alipay", "verification_error"}} {
		if err := s.RecordPaymentActivity(ctx, v.id, v.provider, "notify", v.outcome); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordPaymentActivity(ctx, p.ID, "alipay", "notify", "raw-secret-or-URL"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("unbounded activity data accepted", err)
	}
	recent, err := s.RecentPaymentActivity(ctx)
	if err != nil || len(recent) != 4 {
		t.Fatal(err)
	}
	for _, a := range recent[:3] {
		if a.PaymentID != nil || a.OrderID != nil {
			t.Fatal("untrusted ID bound to financial record")
		}
	}
	linked, err := s.PaymentActivity(ctx, p.ID)
	if err != nil || len(linked) != 1 || linked[0].OrderID == nil || *linked[0].OrderID != o.ID {
		t.Fatal("valid activity link missing", err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE payment_activity SET outcome='paid'`); err == nil {
		t.Fatal("payment evidence mutable")
	}
	if _, err = s.DB.Exec(ctx, `DELETE FROM payment_activity`); err == nil {
		t.Fatal("payment evidence deletable")
	}
	if _, err = s.PaymentActivity(ctx, "unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unknown activity resource accepted", err)
	}
}

func TestLifecycleCompletionAndActivityRollbackTogether(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	_, _, p := paymentFixture(t, s, "alipay")
	if err := s.SchedulePaymentReconciliation(ctx); err != nil {
		t.Fatal(err)
	}
	lifecycleDue(t, s, p.ID)
	j, err := s.ClaimPaymentReconciliation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `CREATE FUNCTION reject_payment_activity() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'activity failure'; END $$; CREATE TRIGGER reject_payment_activity BEFORE INSERT ON payment_activity FOR EACH ROW EXECUTE FUNCTION reject_payment_activity()`); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPaymentReconciliation(ctx, p.ID, j.ClaimToken, "storage_error"); err == nil {
		t.Fatal("injected activity failure ignored")
	}
	var state string
	var attempts int
	if err = s.DB.QueryRow(ctx, `SELECT status,attempts FROM payment_reconciliation WHERE payment_id=$1`, p.ID).Scan(&state, &attempts); err != nil || state != "processing" || attempts != 0 {
		t.Fatal("job completion escaped rollback", err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_payment_activity ON payment_activity`); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPaymentReconciliation(ctx, p.ID, j.ClaimToken, "storage_error"); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleActivityForeignKeysDoNotBlockVerifiedSettlement(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	_, o, p := paymentFixture(t, s, "alipay")
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// An activity writer holds FK KEY SHARE locks on both immutable identities
	// until commit. Financial status writes must not demand key-changing locks.
	if _, err = tx.Exec(ctx, `INSERT INTO payment_activity(payment_id,order_id,provider,source,outcome) VALUES($1,$2,'alipay','query','pending')`, p.ID, o.ID); err != nil {
		t.Fatal(err)
	}
	settleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = s.SettlePayment(settleCtx, p.ID, p.Provider, "metadata-concurrent-transaction", o.Price, o.Currency); err != nil {
		t.Fatal("metadata FK locks blocked financial fulfillment", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
