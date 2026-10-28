package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func actionAdmin(t *testing.T, s *store.Store) string {
	t.Helper()
	id := testutil.User(t, s)
	if _, err := s.DB.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}
func refundFixture(t *testing.T, s *store.Store) (string, string, store.Order, store.Payment, store.Refund) {
	t.Helper()
	ctx := context.Background()
	actor := actionAdmin(t, s)
	u, o, p := paymentFixture(t, s, "alipay")
	if err := s.SettlePayment(ctx, p.ID, p.Provider, "transaction-"+p.ID, o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestRefund(ctx, actor, p.ID, "原订单全额退款")
	if err != nil {
		t.Fatal(err)
	}
	return actor, u, o, p, r
}
func claimAction(t *testing.T, s *store.Store) store.ActionJob {
	t.Helper()
	j, err := s.ClaimPaymentAction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestActionRefundDraftApprovalRolesAndSecurityGeneration(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, _, o, p, r := refundFixture(t, s)
	other := testutil.User(t, s)
	if _, err := s.RequestRefund(ctx, other, p.ID, "unauthorized"); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal(err)
	}
	if err := s.ApproveRefund(ctx, actor, r.ID, 1); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("stale password generation accepted", err)
	}
	if err := s.ApproveRefund(ctx, actor, r.ID, -1); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("missing password generation accepted", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET role='finance' WHERE id=$1`, other); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveRefund(ctx, other, r.ID, 0); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("finance approved", err)
	}
	same, err := s.RequestRefund(ctx, other, p.ID, "different reason")
	if err != nil || same.ID != r.ID || same.Reason != r.Reason {
		t.Fatal("draft identity changed", err)
	}
	if _, err = s.ClaimPaymentAction(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unapproved refund claimed", err)
	}
	var active bool
	if err = s.DB.QueryRow(ctx, `SELECT revoked_at IS NULL FROM entitlement_periods WHERE order_id=$1`, o.ID).Scan(&active); err != nil || !active {
		t.Fatal("draft revoked access", err)
	}
	for i := 0; i < 2; i++ {
		if err = s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	j := claimAction(t, s)
	if j.RefundID != r.ID || j.Amount != o.Price || j.TransactionID != "transaction-"+p.ID {
		t.Fatal("snapshot lost", j)
	}
}
func TestActionRefundSuccessIsAtomicAndPaymentReplayCannotRegrant(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, user, o, p, r := refundFixture(t, s)
	next, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, "development", "future", next.ID, user, next.Price, next.Currency); err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	j := claimAction(t, s)
	if err = s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "succeeded", "provider-refund"); err != nil {
		t.Fatal(err)
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "transaction-"+p.ID, o.Price, o.Currency); err != nil {
		t.Fatal("replayed valid notification rejected", err)
	}
	var status string
	var revoked, nextActive bool
	var events, proofs int
	err = s.DB.QueryRow(ctx, `SELECT o.status,e.revoked_at IS NOT NULL,(SELECT revoked_at IS NULL FROM entitlement_periods WHERE order_id=$2),(SELECT count(*) FROM outbox_events WHERE kind='entitlement.revoked'),(SELECT count(*) FROM payment_events WHERE order_id=$1) FROM orders o JOIN entitlement_periods e ON e.order_id=o.id WHERE o.id=$1`, o.ID, next.ID).Scan(&status, &revoked, &nextActive, &events, &proofs)
	if err != nil || status != "refunded" || !revoked || !nextActive || events != 1 || proofs != 1 {
		t.Fatal("refund facts or access incorrect", status, revoked, nextActive, events, proofs, err)
	}
	got, err := s.Refund(ctx, r.ID)
	if err != nil || got.Status != "succeeded" || got.CompletedAt == nil || got.ProviderRefundID == nil {
		t.Fatal(got, err)
	}
	if err = s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "succeeded", "provider-refund"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("stale completion accepted", err)
	}
}
func TestActionRefundRollbackKeepsAccessAndRecoversWithSameIdentity(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, _, o, _, r := refundFixture(t, s)
	if err := s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	j := claimAction(t, s)
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_refund_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='entitlement.revoked' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_refund BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_refund_event()`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "succeeded", "provider-refund"); err == nil {
		t.Fatal("event failure committed")
	}
	var status string
	var active bool
	if err := s.DB.QueryRow(ctx, `SELECT o.status,e.revoked_at IS NULL FROM orders o JOIN entitlement_periods e ON e.order_id=o.id WHERE o.id=$1`, o.ID).Scan(&status, &active); err != nil || status != "paid" || !active {
		t.Fatal("partial refund transaction", err)
	}
	if _, err := s.DB.Exec(ctx, `DROP TRIGGER fail_refund ON outbox_events`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "succeeded", "provider-refund"); err != nil {
		t.Fatal(err)
	}
}
func TestActionGatewayCloseFencesCheckoutAndPreservesLatePayment(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	user, o, p := paymentFixture(t, s, "wechat")
	if _, err := s.SavePaymentCheckout(ctx, p.ID, "qr", "weixin://wxpay/bizpayurl?pr=test"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPaymentClose(ctx, actor, p.ID, "不再使用", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePayment(ctx, user, o.ID, p.Provider); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("closing checkout reopened", err)
	}
	saved, err := s.SavePaymentCheckout(ctx, p.ID, "qr", "weixin://wxpay/bizpayurl?pr=late")
	if err != nil || saved.CheckoutURL != "" {
		t.Fatal("in-flight checkout exposed", err)
	}
	current, err := s.Payment(ctx, user, o.ID)
	if err != nil || current.CheckoutURL != "" {
		t.Fatal("closing checkout still exposed", err)
	}
	j := claimAction(t, s)
	if err = s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "closed", ""); err != nil {
		t.Fatal(err)
	}
	ledger, err := s.ListPayments(ctx, "", "", o.ID)
	if err != nil || len(ledger) != 1 || ledger[0].ReconcileStatus != "closed" || ledger[0].NextCheckAt != nil {
		t.Fatal("closed payment still schedules queries", ledger, err)
	}
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "late-paid", o.Price, o.Currency); err != nil {
		t.Fatal("verified paid fact lost", err)
	}
	got, err := s.PaymentOrder(ctx, user, o.ID)
	if err != nil || got.Status != "paid" || got.ClosedAt != nil {
		t.Fatal(got, err)
	}
}
func TestActionCloseNotFoundNeverClosesAndCallbackWinsRace(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	user, o, p := paymentFixture(t, s, "alipay")
	if err := s.RequestPaymentClose(ctx, actor, p.ID, "关单核实", 0); err != nil {
		t.Fatal(err)
	}
	j := claimAction(t, s)
	if err := s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "not_found", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.PaymentOrder(ctx, user, o.ID)
	if err != nil || got.Status != "closing" {
		t.Fatal("absence was closure", got, err)
	}
	ledger, err := s.ListPayments(ctx, "", "", o.ID)
	if err != nil || len(ledger) != 1 || ledger[0].LastOutcome != "not_found" || ledger[0].NextCheckAt == nil {
		t.Fatal("close recovery not visible", ledger, err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE payment_action_jobs SET next_attempt_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	j = claimAction(t, s)
	if err = s.SettlePayment(ctx, p.ID, p.Provider, "callback-wins", o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "closed", ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.PaymentOrder(ctx, user, o.ID)
	if err != nil || got.Status != "paid" {
		t.Fatal("close undid payment", err)
	}
}
func TestActionLeaseRecoveryRejectsOldWorkerAndDoesNotDuplicateDraft(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, _, _, p, r := refundFixture(t, s)
	var wg sync.WaitGroup
	ids := make(chan string, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, err := s.RequestRefund(ctx, actor, p.ID, "并发申请")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- x.ID
		}()
	}
	wg.Wait()
	close(ids)
	for id := range ids {
		if id != r.ID {
			t.Fatal("duplicate financial request")
		}
	}
	if err := s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	old := claimAction(t, s)
	if err := s.MarkPaymentActionSubmitted(ctx, old.ID, old.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE payment_action_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	next := claimAction(t, s)
	if next.ID != old.ID || next.RefundID != r.ID || !next.Submitted || next.ClaimToken == old.ClaimToken {
		t.Fatal("unstable recovery", next)
	}
	if err := s.FinishPaymentAction(ctx, old.ID, old.ClaimToken, "succeeded", "old"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("stale worker changed money state", err)
	}
	if err := s.FinishPaymentAction(ctx, next.ID, next.ClaimToken, "pending", ""); err != nil {
		t.Fatal(err)
	}
}
func TestActionBoundedRetriesHorizonAndExplicitReapproval(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, _, _, _, r := refundFixture(t, s)
	if err := s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		j := claimAction(t, s)
		if err := s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "gateway_error", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(ctx, `UPDATE payment_action_jobs SET next_attempt_at=now()-interval '1 second'`); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Refund(ctx, r.ID)
	if err != nil || got.Status != "manual_required" {
		t.Fatal(got, err)
	}
	if _, err = s.ClaimPaymentAction(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("exhausted job reclaimed", err)
	}
	if err = s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE payment_action_jobs SET horizon_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	j := claimAction(t, s)
	if err = s.MarkPaymentActionSubmitted(ctx, j.ID, j.ClaimToken); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("expired action submitted", err)
	}
	if err = s.FinishPaymentAction(ctx, j.ID, j.ClaimToken, "manual_required", ""); err != nil {
		t.Fatal(err)
	}
}
func TestActionUnsupportedChannelsAndUnpaidRefundAreRejected(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, o, p := paymentFixture(t, s, "epay_alipay")
	if err := s.RequestPaymentClose(ctx, actor, p.ID, "关单", 0); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("epay closure accepted", err)
	}
	if _, err := s.RequestRefund(ctx, actor, p.ID, "退款"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("unpaid refund accepted", err)
	}
	if err := s.SettlePayment(ctx, p.ID, p.Provider, "epay-payment", o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestRefund(ctx, actor, p.ID, "退款"); !errors.Is(err, store.ErrPaymentConflict) {
		t.Fatal("unsigned epay action accepted", err)
	}
}
