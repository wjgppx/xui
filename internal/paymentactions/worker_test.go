package paymentactions_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/wjgppx/xui/internal/paymentactions"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type provider struct {
	query       func(payments.Request) (payments.Notification, error)
	refundQuery func(payments.RefundRequest) (payments.RefundResult, error)
	refund      func(payments.RefundRequest) (payments.RefundResult, error)
	close       func(payments.Request) (payments.CloseResult, error)
}

func (*provider) ID() string   { return "alipay" }
func (*provider) Name() string { return "local signed result fixture" }
func (*provider) Create(context.Context, payments.Request) (payments.Checkout, error) {
	return payments.Checkout{}, errors.New("unexpected")
}
func (*provider) Verify(*http.Request) (payments.Notification, error) {
	return payments.Notification{}, payments.ErrVerification
}
func (p *provider) Query(_ context.Context, r payments.Request) (payments.Notification, error) {
	return p.query(r)
}
func (p *provider) QueryRefund(_ context.Context, r payments.RefundRequest) (payments.RefundResult, error) {
	return p.refundQuery(r)
}
func (p *provider) Refund(_ context.Context, r payments.RefundRequest) (payments.RefundResult, error) {
	return p.refund(r)
}
func (p *provider) Close(_ context.Context, r payments.Request) (payments.CloseResult, error) {
	return p.close(r)
}
func fixture(t *testing.T, refund bool) (*store.Store, string, store.Order, store.Payment, store.Refund) {
	t.Helper()
	s := testutil.Database(t)
	ctx := context.Background()
	u := testutil.User(t, s)
	actor := testutil.User(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	o, err := s.CreateOrder(ctx, u, "explore")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePayment(ctx, u, o.ID, "alipay")
	if err != nil {
		t.Fatal(err)
	}
	var r store.Refund
	if refund {
		if err = s.SettlePayment(ctx, p.ID, p.Provider, "transaction-"+p.ID, o.Price, o.Currency); err != nil {
			t.Fatal(err)
		}
		r, err = s.RequestRefund(ctx, actor, p.ID, "本地退款验证")
		if err != nil {
			t.Fatal(err)
		}
		err = s.ApproveRefund(ctx, actor, r.ID, 0)
	} else {
		err = s.RequestPaymentClose(ctx, actor, p.ID, "本地关单验证", 0)
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, u, o, p, r
}
func run(t *testing.T, s *store.Store, p *provider) {
	t.Helper()
	registry, err := payments.New(p)
	if err != nil {
		t.Fatal(err)
	}
	n, err := paymentactions.Process(context.Background(), s, registry, "https://control.example.test", 1)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
func result(r payments.RefundRequest) payments.RefundResult {
	return payments.RefundResult{PaymentID: r.PaymentID, TransactionID: r.TransactionID, RefundID: r.RefundID, ProviderRefundID: "provider-refund", Amount: r.Amount, Currency: r.Currency, Status: "succeeded"}
}
func due(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(), `UPDATE payment_action_jobs SET next_attempt_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func TestRefundUncertaintyNeverSubmitsUntilSignedAbsence(t *testing.T) {
	for _, failure := range []error{payments.ErrGateway, payments.ErrVerification, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			s, _, o, _, r := fixture(t, true)
			submits := 0
			p := &provider{refundQuery: func(payments.RefundRequest) (payments.RefundResult, error) { return payments.RefundResult{}, failure }, refund: func(q payments.RefundRequest) (payments.RefundResult, error) { submits++; return result(q), nil }}
			run(t, s, p)
			got, err := s.Refund(context.Background(), r.ID)
			if err != nil || got.Status != "queued" || submits != 0 {
				t.Fatal("uncertainty moved money", got, submits, err)
			}
			var active bool
			if err = s.DB.QueryRow(context.Background(), `SELECT revoked_at IS NULL FROM entitlement_periods WHERE order_id=$1`, o.ID).Scan(&active); err != nil || !active {
				t.Fatal("uncertainty revoked rights", err)
			}
		})
	}
}
func TestRefundTimeoutRecoversByQueryWithoutSecondSubmission(t *testing.T) {
	s, _, _, payment, r := fixture(t, true)
	queries, submits := 0, 0
	p := &provider{refundQuery: func(q payments.RefundRequest) (payments.RefundResult, error) {
		queries++
		if q.RefundID != r.ID || q.PaymentID != payment.ID {
			t.Error("identity changed")
		}
		if queries == 1 {
			return payments.RefundResult{}, payments.ErrNotPaid
		}
		return result(q), nil
	}, refund: func(q payments.RefundRequest) (payments.RefundResult, error) {
		submits++
		return payments.RefundResult{}, context.DeadlineExceeded
	}}
	run(t, s, p)
	due(t, s)
	run(t, s, p)
	got, err := s.Refund(context.Background(), r.ID)
	if err != nil || got.Status != "succeeded" || queries != 2 || submits != 1 {
		t.Fatal(got, queries, submits, err)
	}
}
func TestRefundExactSnapshotAndPendingDoNotRevokeAccess(t *testing.T) {
	for _, mode := range []string{"amount", "transaction", "refund", "currency", "provider_id", "processing", "failed", "manual_required"} {
		t.Run(mode, func(t *testing.T) {
			s, _, o, _, r := fixture(t, true)
			submits := 0
			p := &provider{refundQuery: func(q payments.RefundRequest) (payments.RefundResult, error) {
				v := result(q)
				switch mode {
				case "amount":
					v.Amount++
				case "transaction":
					v.TransactionID = "wrong"
				case "refund":
					v.RefundID = "wrong"
				case "currency":
					v.Currency = "USD"
				case "provider_id":
					v.ProviderRefundID = ""
				default:
					v.Status = mode
				}
				return v, nil
			}, refund: func(q payments.RefundRequest) (payments.RefundResult, error) { submits++; return result(q), nil }}
			run(t, s, p)
			got, err := s.Refund(context.Background(), r.ID)
			if err != nil || got.Status == "succeeded" || submits != 0 {
				t.Fatal("unverified refund completed", got, err)
			}
			var active bool
			if err = s.DB.QueryRow(context.Background(), `SELECT revoked_at IS NULL FROM entitlement_periods WHERE order_id=$1`, o.ID).Scan(&active); err != nil || !active {
				t.Fatal("incomplete refund revoked rights", err)
			}
		})
	}
}
func TestCloseFirstRecoversPaymentAndOtherwiseNeedsPositiveClosure(t *testing.T) {
	for _, mode := range []string{"paid", "not_found", "closed", "already_closed", "bad_identity", "query_failure"} {
		t.Run(mode, func(t *testing.T) {
			s, u, o, _, _ := fixture(t, false)
			closes := 0
			p := &provider{query: func(q payments.Request) (payments.Notification, error) {
				if mode == "query_failure" {
					return payments.Notification{}, payments.ErrGateway
				}
				if mode == "already_closed" {
					return payments.Notification{PaymentID: q.PaymentID, Amount: q.Amount, Currency: q.Currency, Closed: true}, nil
				}
				if mode == "paid" {
					return payments.Notification{PaymentID: q.PaymentID, Amount: q.Amount, Currency: q.Currency, TransactionID: "recovered-before-close", Paid: true}, nil
				}
				return payments.Notification{}, payments.ErrNotPaid
			}, close: func(q payments.Request) (payments.CloseResult, error) {
				closes++
				if mode == "not_found" {
					return payments.CloseResult{}, payments.ErrNotPaid
				}
				v := payments.CloseResult{PaymentID: q.PaymentID, Status: "closed"}
				if mode == "bad_identity" {
					v.PaymentID = "wrong"
				}
				return v, nil
			}}
			run(t, s, p)
			got, err := s.PaymentOrder(context.Background(), u, o.ID)
			want := "closing"
			if mode == "paid" {
				want = "paid"
			}
			if mode == "closed" || mode == "already_closed" {
				want = "closed"
			}
			if err != nil || got.Status != want {
				t.Fatal(mode, got.Status, err)
			}
			if (mode == "paid" || mode == "query_failure" || mode == "already_closed") && closes != 0 {
				t.Fatal("unsafe close call", mode)
			}
		})
	}
}
func TestExpiredActionHorizonStopsAllGatewayCalls(t *testing.T) {
	s, _, _, _, r := fixture(t, true)
	if _, err := s.DB.Exec(context.Background(), `UPDATE payment_action_jobs SET horizon_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	p := &provider{refundQuery: func(q payments.RefundRequest) (payments.RefundResult, error) {
		t.Error("expired action reached provider")
		return result(q), nil
	}}
	run(t, s, p)
	got, err := s.Refund(context.Background(), r.ID)
	if err != nil || got.Status != "manual_required" {
		t.Fatal(got, err)
	}
}
