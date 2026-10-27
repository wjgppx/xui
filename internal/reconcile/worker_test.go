package reconcile_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/reconcile"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type provider struct {
	id    string
	query func(context.Context, payments.Request) (payments.Notification, error)
}

func (p *provider) ID() string   { return p.id }
func (p *provider) Name() string { return "local query fixture" }
func (p *provider) Create(context.Context, payments.Request) (payments.Checkout, error) {
	return payments.Checkout{}, errors.New("unexpected create")
}
func (p *provider) Verify(*http.Request) (payments.Notification, error) {
	return payments.Notification{}, payments.ErrVerification
}
func (p *provider) Query(c context.Context, r payments.Request) (payments.Notification, error) {
	return p.query(c, r)
}
func registry(t *testing.T, p *provider) *payments.Registry {
	t.Helper()
	r, e := payments.New(p)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func intent(t *testing.T, s *store.Store, id string) (string, store.Order, store.Payment) {
	t.Helper()
	ctx := context.Background()
	u := testutil.User(t, s)
	o, e := s.CreateOrder(ctx, u, "explore")
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.CreatePayment(ctx, u, o.ID, id)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SchedulePaymentReconciliation(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE payment_reconciliation SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, p.ID); e != nil {
		t.Fatal(e)
	}
	return u, o, p
}
func paid(r payments.Request) payments.Notification {
	return payments.Notification{PaymentID: r.PaymentID, TransactionID: "transaction-" + r.PaymentID, Amount: r.Amount, Currency: r.Currency, Paid: true}
}
func run(t *testing.T, s *store.Store, r *payments.Registry) int {
	t.Helper()
	n, e := reconcile.Process(context.Background(), s, r, "https://control.example.test", 1)
	if e != nil {
		t.Fatal(e)
	}
	return n
}

func TestLostNotificationRecoveredFromVerifiedQueryOnce(t *testing.T) {
	s := testutil.Database(t)
	u, o, p := intent(t, s, "alipay")
	var calls atomic.Int32
	r := registry(t, &provider{id: "alipay", query: func(_ context.Context, req payments.Request) (payments.Notification, error) {
		calls.Add(1)
		if req.PaymentID != p.ID || req.OrderID != o.ID || req.Amount != 1900 || req.Currency != "CNY" || req.NotifyURL != "https://control.example.test/api/v1/payments/notify/alipay" {
			t.Error("query lost immutable snapshot")
		}
		return paid(req), nil
	}})
	if run(t, s, r) != 1 || run(t, s, r) != 0 || calls.Load() != 1 {
		t.Fatal("reconciliation was not bounded/idempotent")
	}
	payment, e := s.Payment(context.Background(), u, o.ID)
	if e != nil || payment.Status != "paid" {
		t.Fatal("lost payment not fulfilled", e)
	}
	var periods, activities int
	if e = s.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM entitlement_periods WHERE order_id=$1),(SELECT count(*) FROM payment_activity WHERE payment_id=$2 AND source='auto_query' AND outcome='paid')`, o.ID, p.ID).Scan(&periods, &activities); e != nil {
		t.Fatal(e)
	}
	if periods != 1 || activities != 1 {
		t.Fatal("fulfillment or finite activity duplicated", periods, activities)
	}
}

func TestUntrustedQueryResultsCannotFulfillAndAreClassified(t *testing.T) {
	for _, kind := range []string{"wrong_id", "wrong_amount", "wrong_currency", "bad_signature", "gateway", "not_paid"} {
		t.Run(kind, func(t *testing.T) {
			s := testutil.Database(t)
			_, o, p := intent(t, s, "wechat")
			r := registry(t, &provider{id: "wechat", query: func(_ context.Context, q payments.Request) (payments.Notification, error) {
				n := paid(q)
				switch kind {
				case "wrong_id":
					n.PaymentID = "different"
				case "wrong_amount":
					n.Amount++
				case "wrong_currency":
					n.Currency = "USD"
				case "bad_signature":
					return n, payments.ErrVerification
				case "gateway":
					return n, payments.ErrGateway
				case "not_paid":
					return n, payments.ErrNotPaid
				}
				return n, nil
			}})
			if run(t, s, r) != 1 {
				t.Fatal("query was not processed")
			}
			var outcome string
			var attempts, periods int
			if e := s.DB.QueryRow(context.Background(), `SELECT j.last_outcome,j.attempts,(SELECT count(*) FROM entitlement_periods WHERE order_id=$2) FROM payment_reconciliation j WHERE payment_id=$1`, p.ID, o.ID).Scan(&outcome, &attempts, &periods); e != nil {
				t.Fatal(e)
			}
			want := "verification_error"
			if kind == "gateway" {
				want = "gateway_error"
			}
			if kind == "not_paid" {
				want = "not_paid"
			}
			if outcome != want || periods != 0 || (kind == "not_paid" && attempts != 0) || (kind != "not_paid" && attempts != 1) {
				t.Fatal("query result misclassified", outcome, attempts, periods)
			}
		})
	}
}

func TestCallbackWinningAgainstPendingQueryIsNotDowngraded(t *testing.T) {
	s := testutil.Database(t)
	_, o, p := intent(t, s, "alipay")
	if _, e := s.DB.Exec(context.Background(), `UPDATE orders SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, o.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.SchedulePaymentReconciliation(context.Background()); e != nil {
		t.Fatal(e)
	}
	r := registry(t, &provider{id: "alipay", query: func(c context.Context, q payments.Request) (payments.Notification, error) {
		n := paid(q)
		if e := s.SettlePayment(c, p.ID, "alipay", n.TransactionID, n.Amount, n.Currency); e != nil {
			return n, e
		}
		n.Paid = false
		return n, nil
	}})
	if run(t, s, r) != 1 {
		t.Fatal("query missing")
	}
	var orderState, jobState, outcome string
	if e := s.DB.QueryRow(context.Background(), `SELECT o.status,j.status,j.last_outcome FROM orders o JOIN payment_intents p ON p.order_id=o.id JOIN payment_reconciliation j ON j.payment_id=p.id WHERE o.id=$1`, o.ID).Scan(&orderState, &jobState, &outcome); e != nil {
		t.Fatal(e)
	}
	if orderState != "paid" || jobState != "paid" || outcome != "paid" {
		t.Fatal("stale pending result downgraded paid fact", orderState, jobState, outcome)
	}
}

func TestExpiredWorkerLeaseCannotOverwriteRecoveredPayment(t *testing.T) {
	s := testutil.Database(t)
	_, _, p := intent(t, s, "wechat")
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	r := registry(t, &provider{id: "wechat", query: func(c context.Context, q payments.Request) (payments.Notification, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-c.Done():
				return payments.Notification{}, c.Err()
			}
			n := paid(q)
			n.Paid = false
			return n, nil
		}
		return paid(q), nil
	}})
	result := make(chan error, 1)
	go func() {
		_, e := reconcile.Process(context.Background(), s, r, "https://control.example.test", 1)
		result <- e
	}()
	<-entered
	if _, e := s.DB.Exec(context.Background(), `UPDATE payment_reconciliation SET lease_until=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, p.ID); e != nil {
		close(release)
		t.Fatal(e)
	}
	if run(t, s, r) != 1 {
		close(release)
		t.Fatal("expired lease not recovered")
	}
	close(release)
	if e := <-result; e != nil {
		t.Fatal(e)
	}
	var state string
	var checks int
	if e := s.DB.QueryRow(context.Background(), `SELECT status,checks FROM payment_reconciliation WHERE payment_id=$1`, p.ID).Scan(&state, &checks); e != nil {
		t.Fatal(e)
	}
	if state != "paid" || checks != 2 {
		t.Fatal("stale lease overwrote recovery", state, checks)
	}
}

func TestStorageFailureIsRetriedWithoutFalseFulfillment(t *testing.T) {
	s := testutil.Database(t)
	_, o, p := intent(t, s, "alipay")
	ctx := context.Background()
	if _, e := s.DB.Exec(ctx, `CREATE FUNCTION reject_payment_fulfillment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private-provider-error-must-not-be-recorded'; END $$; CREATE TRIGGER reject_fulfillment BEFORE INSERT ON entitlement_periods FOR EACH ROW EXECUTE FUNCTION reject_payment_fulfillment()`); e != nil {
		t.Fatal(e)
	}
	r := registry(t, &provider{id: "alipay", query: func(_ context.Context, q payments.Request) (payments.Notification, error) { return paid(q), nil }})
	run(t, s, r)
	var outcome, state string
	if e := s.DB.QueryRow(ctx, `SELECT j.last_outcome,o.status FROM payment_reconciliation j JOIN payment_intents p ON p.id=j.payment_id JOIN orders o ON o.id=p.order_id WHERE j.payment_id=$1`, p.ID).Scan(&outcome, &state); e != nil {
		t.Fatal(e)
	}
	if outcome != "storage_error" || state != "pending" {
		t.Fatal("storage failure concealed", outcome, state)
	}
	if _, e := s.DB.Exec(ctx, `DROP TRIGGER reject_fulfillment ON entitlement_periods`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE payment_reconciliation SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, p.ID); e != nil {
		t.Fatal(e)
	}
	run(t, s, r)
	var periods int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM entitlement_periods WHERE order_id=$1`, o.ID).Scan(&periods); e != nil || periods != 1 {
		t.Fatal("retry did not fulfill exactly once", e, periods)
	}
}

func TestEpayIsNotQueriedAndDisabledDirectChannelNeedsReview(t *testing.T) {
	s := testutil.Database(t)
	intent(t, s, "epay_alipay")
	if run(t, s, nil) != 0 {
		t.Fatal("unsigned Epay query scheduled")
	}
	_, _, p := intent(t, s, "wechat")
	if run(t, s, nil) != 1 {
		t.Fatal("missing provider not classified")
	}
	var outcome string
	if e := s.DB.QueryRow(context.Background(), `SELECT last_outcome FROM payment_reconciliation WHERE payment_id=$1`, p.ID).Scan(&outcome); e != nil {
		t.Fatal(e)
	}
	if outcome != "provider_unavailable" {
		t.Fatal("disabled provider misclassified", outcome)
	}
}

func TestShutdownLeavesRecoverableLeaseWithoutRecordingFalseFailure(t *testing.T) {
	s := testutil.Database(t)
	_, _, p := intent(t, s, "wechat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	r := registry(t, &provider{id: "wechat", query: func(c context.Context, _ payments.Request) (payments.Notification, error) {
		close(entered)
		<-c.Done()
		return payments.Notification{}, c.Err()
	}})
	result := make(chan error, 1)
	go func() { _, err := reconcile.Process(ctx, s, r, "https://control.example.test", 1); result <- err }()
	select {
	case <-entered:
	case err := <-result:
		t.Fatal("worker stopped before query", err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("worker ignored shutdown", err)
	}
	var state string
	var attempts int
	if err := s.DB.QueryRow(context.Background(), `SELECT status,attempts FROM payment_reconciliation WHERE payment_id=$1`, p.ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "processing" || attempts != 0 {
		t.Fatal("shutdown converted into payment failure", state, attempts)
	}
	if _, err := s.DB.Exec(context.Background(), `UPDATE payment_reconciliation SET lease_until=clock_timestamp()-interval '1 second' WHERE payment_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	r = registry(t, &provider{id: "wechat", query: func(_ context.Context, q payments.Request) (payments.Notification, error) { return paid(q), nil }})
	if run(t, s, r) != 1 {
		t.Fatal("shutdown lease was not recovered")
	}
}
