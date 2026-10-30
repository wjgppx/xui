package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func paymentFixture(t *testing.T, s *store.Store, provider string) (string, store.Order, store.Payment) {
	t.Helper()
	ctx := context.Background()
	user := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePayment(ctx, user, o.ID, provider)
	if err != nil {
		t.Fatal(err)
	}
	return user, o, p
}

func TestPaymentSettlementPreservesTransientUserLockErrors(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, order, payment := paymentFixture(t, s, "alipay")
	blocker, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, user); err != nil {
		t.Fatal(err)
	}
	// A server-side lock timeout proves the settlement reached the user lock;
	// a client deadline alone could instead expire before acquiring a connection.
	config := s.DB.Config()
	config.ConnConfig.RuntimeParams["lock_timeout"] = "100ms"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("isolated lock-timeout pool unavailable")
	}
	defer pool.Close()
	timed := &store.Store{DB: pool}
	for _, test := range []struct {
		name   string
		settle func() error
	}{
		{"development", func() error {
			return timed.Settle(ctx, "development", "lock-timeout", order.ID, user, order.Price, order.Currency)
		}},
		{"verified-intent", func() error {
			return timed.SettlePayment(ctx, payment.ID, payment.Provider, "lock-timeout", order.Price, order.Currency)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.settle()
			var failure *pgconn.PgError
			if errors.Is(err, store.ErrNotFound) || !errors.As(err, &failure) || failure.Code != "55P03" {
				t.Fatal("transient database lock failure was hidden as a missing payment or order")
			}
		})
	}
	var status string
	if err = s.DB.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, order.ID).Scan(&status); err != nil || status != "pending" {
		t.Fatal("failed settlement changed the order")
	}
	for _, table := range []string{"payment_events", "outbox_events", "entitlement_periods"} {
		var count int
		if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed settlement partially committed financial state", table)
		}
	}
}

func TestPaymentIntentConcurrentCreationPinsProviderAndOwner(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		payment store.Payment
		err     error
	}
	results := make(chan result, 10)
	for i := 0; i < 10; i++ {
		go func() { p, err := s.CreatePayment(ctx, user, o.ID, "alipay"); results <- result{p, err} }()
	}
	id := ""
	for i := 0; i < 10; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if id == "" {
			id = r.payment.ID
		}
		if r.payment.ID != id || r.payment.Status != "pending" {
			t.Fatal("concurrent creation changed merchant identity")
		}
	}
	if _, err = s.CreatePayment(ctx, user, o.ID, "wechat"); !errors.Is(err, store.ErrPaymentProviderConflict) {
		t.Fatal("provider changed", err)
	}
	other := testutil.User(t, s)
	if _, err = s.CreatePayment(ctx, other, o.ID, "alipay"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-owner creation", err)
	}
	if _, err = s.Payment(ctx, other, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-owner status", err)
	}
	if _, err = s.PaymentOrder(ctx, other, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-owner snapshot", err)
	}
	got, err := s.PaymentOrder(ctx, user, o.ID)
	if err != nil || got.Price != o.Price || got.UserID != user {
		t.Fatal("wrong snapshot", err)
	}
}

func TestPaymentCreationRequiresVerificationAndPendingOrder(t *testing.T) {
	s := testutil.Database(t)
	s.RequireVerified = true
	ctx := context.Background()
	user := testutil.User(t, s)
	o, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePayment(ctx, user, o.ID, "alipay"); !errors.Is(err, store.ErrEmailUnverified) {
		t.Fatal("unverified checkout", err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE users SET email_verified_at=clock_timestamp() WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePayment(ctx, user, o.ID, "alipay")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "paid-transaction", o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePayment(ctx, user, o.ID, "alipay"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("paid order reopened", err)
	}
	if _, err = s.CreatePayment(ctx, user, o.ID, "unsupported"); !errors.Is(err, store.ErrPaymentProviderConflict) {
		t.Fatal("unknown provider", err)
	}
}

func TestPaymentSettlementRejectsMismatchAndConcurrentReplayGrantsOnce(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, o, p := paymentFixture(t, s, "alipay")
	for _, value := range []struct {
		amount   int64
		currency string
	}{{1, "CNY"}, {o.Price, "USD"}} {
		if err := s.SettlePayment(ctx, p.ID, p.Provider, "transaction", value.amount, value.currency); !errors.Is(err, store.ErrPaymentConflict) {
			t.Fatal("amount/currency mismatch", err)
		}
	}
	if err := s.SettlePayment(ctx, p.ID, "wechat", "transaction", o.Price, o.Currency); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("provider mismatch", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE plans SET price_minor=9999,quota_bytes=1,name='changed' WHERE id='explore'`); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() { results <- s.SettlePayment(ctx, p.ID, p.Provider, "transaction", o.Price, o.Currency) }()
	}
	for i := 0; i < 10; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Payment(ctx, user, o.ID)
	if err != nil || got.Status != "paid" || got.TransactionID == nil || *got.TransactionID != "transaction" || got.PaidAt == nil {
		t.Fatal("intent not paid", err)
	}
	periods, err := s.Entitlements(ctx, user)
	if err != nil || len(periods) != 1 || periods[0].Quota != o.Quota || periods[0].PlanName != o.PlanName {
		t.Fatal("purchase snapshot lost or fulfilled twice", err)
	}
	for _, table := range []string{"payment_events", "outbox_events", "entitlement_periods"} {
		var count int
		if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			t.Fatal(table, count, err)
		}
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "different-transaction", o.Price, o.Currency); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("second provider transaction accepted", err)
	}
}

func TestPaymentCheckoutFirstWinsAndPaidCallbackCannotBeDowngraded(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, o, p := paymentFixture(t, s, "wechat")
	type result struct {
		payment store.Payment
		err     error
	}
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			got, err := s.SavePaymentCheckout(ctx, p.ID, "qr", fmt.Sprintf("weixin://wxpay/bizpayurl?pr=%d", i))
			results <- result{got, err}
		}(i)
	}
	link := ""
	for i := 0; i < 8; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if link == "" {
			link = r.payment.CheckoutURL
		}
		if r.payment.CheckoutURL != link || r.payment.Status != "ready" {
			t.Fatal("checkout changed between callers")
		}
	}
	if err := s.SettlePayment(ctx, p.ID, p.Provider, "wx-transaction", o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	got, err := s.SavePaymentCheckout(ctx, p.ID, "qr", "weixin://wxpay/bizpayurl?pr=late")
	if err != nil || got.Status != "paid" || got.CheckoutURL != link {
		t.Fatal("late save downgraded paid intent", err)
	}
	_, other, early := paymentFixture(t, s, "alipay")
	if err = s.SettlePayment(ctx, early.ID, early.Provider, "callback-before-response", other.Price, other.Currency); err != nil {
		t.Fatal(err)
	}
	got, err = s.SavePaymentCheckout(ctx, early.ID, "redirect", "https://pay.example.test/late")
	if err != nil || got.Status != "paid" || got.CheckoutURL != "" {
		t.Fatal("early callback lost", err)
	}
	if _, err = s.SavePaymentCheckout(ctx, p.ID, "redirect", "javascript:alert(1)"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("unsafe checkout", err)
	}
	if _, err = s.Payment(ctx, user, o.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPaymentIntentAndBusinessFulfillmentRollbackTogether(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, o, p := paymentFixture(t, s, "epay_alipay")
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_payment_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected payment failure'; END $$; CREATE TRIGGER reject_payment_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_payment_outbox()`); err != nil {
		t.Fatal(err)
	}
	if err := s.SettlePayment(ctx, p.ID, p.Provider, "epay-transaction", o.Price, o.Currency); err == nil {
		t.Fatal("injected settlement succeeded")
	}
	got, err := s.Payment(ctx, user, o.ID)
	if err != nil || got.Status != "pending" || got.TransactionID != nil {
		t.Fatal("intent escaped rollback", err)
	}
	order, err := s.PaymentOrder(ctx, user, o.ID)
	if err != nil || order.Status != "pending" {
		t.Fatal("order escaped rollback", err)
	}
	for _, table := range []string{"payment_events", "outbox_events", "entitlement_periods"} {
		var count int
		if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_payment_outbox ON outbox_events`); err != nil {
		t.Fatal(err)
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "epay-transaction", o.Price, o.Currency); err != nil {
		t.Fatal("retry after rollback", err)
	}
}

func TestPaymentProviderTransactionCannotBeReusedForAnotherOrder(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	_, first, p := paymentFixture(t, s, "epay_wechat")
	user, second, q := paymentFixture(t, s, "epay_wechat")
	if err := s.SettlePayment(ctx, p.ID, p.Provider, "shared-provider-transaction", first.Price, first.Currency); err != nil {
		t.Fatal(err)
	}
	if err := s.SettlePayment(ctx, q.ID, q.Provider, "shared-provider-transaction", second.Price, second.Currency); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("transaction reused across orders", err)
	}
	got, err := s.Payment(ctx, user, second.ID)
	if err != nil || got.Status != "pending" {
		t.Fatal("conflicting intent changed", err)
	}
	periods, err := s.Entitlements(ctx, user)
	if err != nil || len(periods) != 0 {
		t.Fatal("conflict granted rights", err)
	}
}

func TestPaymentAndLegacySettlementSerializeRenewals(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user, o, p := paymentFixture(t, s, "alipay")
	second, err := s.CreateOrder(ctx, user, "everyday")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results <- s.SettlePayment(ctx, p.ID, p.Provider, "first-txn", o.Price, o.Currency)
	}()
	go func() {
		defer wg.Done()
		results <- s.Settle(ctx, "development", "second-txn", second.ID, user, second.Price, second.Currency)
	}()
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	periods, err := s.Entitlements(ctx, user)
	if err != nil || len(periods) != 2 || !periods[0].EndsAt.Equal(periods[1].StartsAt) {
		t.Fatal("payment paths overlapped renewal periods", err)
	}
}
