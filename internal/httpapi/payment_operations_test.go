package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

func TestPaymentOperationsCloseOnlyOwnOrdersWithoutGatewayIntent(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"})
	u, cookie := register(t, f)
	_, other := register(t, f)
	o := paymentOrder(t, f, u)
	path := "/api/v1/orders/" + o.ID + "/close"
	for _, tc := range []struct {
		cookie *http.Cookie
		status int
	}{{nil, 401}, {other, 404}} {
		if code, _, _ := request(t, f, "POST", path, "{}", tc.cookie); code != tc.status {
			t.Fatal("close ownership", code)
		}
	}
	for i := 0; i < 2; i++ {
		code, body, _ := request(t, f, "POST", path, "{}", cookie)
		var closed store.Order
		if code != 200 || json.Unmarshal(body, &closed) != nil || closed.Status != "closed" || closed.ClosedAt == nil {
			t.Fatalf("close %d %s", code, body)
		}
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/dev/orders/"+o.ID+"/settle", "{}", cookie); code != 409 {
		t.Fatal("closed order test-payment bypass", code)
	}
	withGateway := paymentOrder(t, f, u)
	paymentCreate(t, f, withGateway, cookie, "alipay")
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE orders SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, withGateway.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/orders/"+withGateway.ID+"/close", "{}", cookie); code != 409 {
		t.Fatal("locally closed a remote payment", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/orders/"+withGateway.ID+"/payment", `{"provider":"alipay"}`, cookie); code != 409 {
		t.Fatal("expired checkout accepted", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/dev/orders/"+withGateway.ID+"/settle", "{}", cookie); code != 409 {
		t.Fatal("expired order test-payment bypass", code)
	}
	var closes int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM payment_activity WHERE order_id=$1 AND source='close' AND outcome='closed'`, o.ID).Scan(&closes); err != nil || closes != 1 {
		t.Fatal("close activity missing or duplicated", err, closes)
	}
}

func TestPaymentOperationsStaffPermissionsAndSummaryRedaction(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	paths := []string{"/api/v1/admin/payments", "/api/v1/admin/payments/" + p.ID + "/activity", "/api/v1/admin/payment-activity"}
	for _, path := range paths {
		if code, _, _ := request(t, f, "GET", path, "", nil); code != 401 {
			t.Fatal("anonymous staff access", code)
		}
	}
	for _, role := range []string{"user", "support", "operator", "finance", "admin"} {
		_, cookie := staffAccount(t, f, role)
		want := 403
		if role == "admin" || role == "finance" {
			want = 200
		}
		for _, path := range paths {
			code, body, _ := request(t, f, "GET", path, "", cookie)
			if code != want {
				t.Fatalf("%s %s got %d %s", role, path, code, body)
			}
			if code == 200 && (strings.Contains(string(body), "checkout_url") || strings.Contains(string(body), "https://pay.example.test") || strings.Contains(string(body), "password_hash")) {
				t.Fatal("staff payment summary leaked checkout or authentication data")
			}
		}
		if want == 403 {
			if code, _, _ := request(t, f, "POST", "/api/v1/admin/payments/"+p.ID+"/retry", "{}", cookie); code != 403 {
				t.Fatal("unprivileged reconciliation retry", role, code)
			}
		}
	}
}

func TestPaymentOperationsFiltersAndRetryDoNotCreatePaymentProof(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"}, &paymentFake{id: "epay_alipay"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	epOrder := paymentOrder(t, f, u)
	ep := paymentCreate(t, f, epOrder, c, "epay_alipay")
	_, finance := staffAccount(t, f, "finance")
	path := "/api/v1/admin/payments"
	code, body, _ := request(t, f, "GET", path+"?status=ready&provider=alipay&order_id="+o.ID, "", finance)
	var list struct {
		Items []store.PaymentOverview `json:"items"`
	}
	if code != 200 || json.Unmarshal(body, &list) != nil || len(list.Items) != 1 || list.Items[0].ID != p.ID || list.Items[0].Amount != o.Price {
		t.Fatalf("payment filters %d %s", code, body)
	}
	for _, query := range []string{"?status=bogus", "?provider=other", "?order_id='OR1=1", "?provider=alipay&provider=wechat", "?sort=checkout_url", "?bad=%ZZ"} {
		if code, _, _ := request(t, f, "GET", path+query, "", finance); code != 400 {
			t.Fatal("invalid payment filter accepted", query, code)
		}
	}
	if code, body, _ := request(t, f, "POST", path+"/"+p.ID+"/retry", "{}", finance); code != 200 {
		t.Fatalf("retry %d %s", code, body)
	}
	if code, _, _ := request(t, f, "POST", path+"/"+ep.ID+"/retry", "{}", finance); code != 409 {
		t.Fatal("Epay unsigned query requeued", code)
	}
	if code, _, _ := request(t, f, "POST", path+"/"+p.ID+"/retry", `{"paid":true}`, finance); code != 400 {
		t.Fatal("manual payment flag accepted", code)
	}
	if a, b, c := paymentCounts(t, f, o.ID); a+b+c != 0 {
		t.Fatal("retry granted entitlement without payment proof")
	}
	var audits int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='payment.reconciliation_retried' AND resource_id=$1`, p.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("retry audit missing", err, audits)
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE payment_reconciliation SET status='manual_required',last_outcome='gateway_error',attempts=8 WHERE payment_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	code, body, _ = request(t, f, "GET", path+"?status=manual_required", "", finance)
	if code != 200 || json.Unmarshal(body, &list) != nil || len(list.Items) != 1 || list.Items[0].ID != p.ID {
		t.Fatalf("manual-required filter %d %s", code, body)
	}
}

func TestPaymentOperationsNotificationsRecordOnlyFiniteTrustedMetadata(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"}, &paymentFake{id: "wechat"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	_, finance := staffAccount(t, f, "finance")
	n := payments.Notification{PaymentID: p.ID, TransactionID: "external-transaction-do-not-store-in-activity", Amount: o.Price, Currency: o.Currency, Paid: true}
	if code, _ := paymentCallback(t, f, "alipay", n, false); code != 400 {
		t.Fatal("forgery accepted", code)
	}
	if code, _ := paymentCallback(t, f, "wechat", n, true); code != 400 {
		t.Fatal("wrong channel accepted", code)
	}
	wrong := n
	wrong.Amount++
	if code, _ := paymentCallback(t, f, "alipay", wrong, true); code != 400 {
		t.Fatal("wrong amount accepted", code)
	}
	if code, _ := paymentCallback(t, f, "alipay", n, true); code != 200 {
		t.Fatal("valid payment rejected", code)
	}
	code, body, _ := request(t, f, "GET", "/api/v1/admin/payment-activity", "", finance)
	var activities []map[string]json.RawMessage
	if code != 200 || json.Unmarshal(body, &activities) != nil {
		t.Fatalf("activity %d %s", code, body)
	}
	allowed := map[string]bool{"id": true, "payment_id": true, "order_id": true, "provider": true, "source": true, "outcome": true, "created_at": true}
	unboundFailures, boundConflicts, paid := 0, 0, 0
	for _, a := range activities {
		for key := range a {
			if !allowed[key] {
				t.Fatal("unexpected payment activity field", key)
			}
		}
		var outcome string
		_ = json.Unmarshal(a["outcome"], &outcome)
		switch outcome {
		case "verification_error":
			if string(a["payment_id"]) != "null" || string(a["order_id"]) != "null" {
				t.Fatal("unverified body chose audit identity")
			}
			unboundFailures++
		case "conflict":
			if string(a["payment_id"]) != "null" {
				boundConflicts++
			}
		case "paid":
			paid++
		}
	}
	if unboundFailures != 1 || boundConflicts != 1 || paid != 1 {
		t.Fatal("payment activity categories wrong", unboundFailures, boundConflicts, paid)
	}
	if strings.Contains(string(body), n.TransactionID) || strings.Contains(string(body), "Signature") || strings.Contains(string(body), "checkout_url") {
		t.Fatal("raw financial proof leaked in activity")
	}
}

func TestPaymentOperationsManualQueryRefreshesConcurrentCallbackState(t *testing.T) {
	pv := &paymentFake{id: "alipay"}
	f := paymentSetup(t, false, pv)
	u, c := register(t, f)
	for _, notFound := range []bool{false, true} {
		o := paymentOrder(t, f, u)
		p := paymentCreate(t, f, o, c, "alipay")
		pv.query = func(r payments.Request) (payments.Notification, error) {
			if err := f.db.SettlePayment(context.Background(), p.ID, p.Provider, "concurrent-"+p.ID, o.Price, o.Currency); err != nil {
				return payments.Notification{}, err
			}
			if notFound {
				return payments.Notification{}, payments.ErrNotPaid
			}
			return payments.Notification{PaymentID: r.PaymentID, Amount: r.Amount, Currency: r.Currency, Paid: false}, nil
		}
		code, body, _ := request(t, f, "POST", "/api/v1/orders/"+o.ID+"/payment/sync", "{}", c)
		var latest store.Payment
		if code != 200 || json.Unmarshal(body, &latest) != nil || latest.Status != "paid" {
			t.Fatalf("stale pending query overwrote callback result %d %s", code, body)
		}
	}
}

func TestPaymentOperationsQueryFailuresAreClassifiedWithoutLeakingErrors(t *testing.T) {
	pv := &paymentFake{id: "alipay"}
	f := paymentSetup(t, false, pv)
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	for _, tc := range []struct {
		err     error
		outcome string
	}{{payments.ErrVerification, "verification_error"}, {errors.New("private-provider-response-secret"), "gateway_error"}} {
		pv.query = func(payments.Request) (payments.Notification, error) { return payments.Notification{}, tc.err }
		code, body, _ := request(t, f, "POST", "/api/v1/orders/"+o.ID+"/payment/sync", "{}", c)
		if code != 502 || strings.Contains(string(body), "private-provider-response-secret") {
			t.Fatalf("query failure %d %s", code, body)
		}
		var count int
		if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM payment_activity WHERE payment_id=$1 AND source='query' AND outcome=$2`, p.ID, tc.outcome).Scan(&count); err != nil || count != 1 {
			t.Fatal("query error not classified", tc.outcome, err, count)
		}
	}
	if a, b, c := paymentCounts(t, f, o.ID); a+b+c != 0 {
		t.Fatal("query failure granted rights")
	}
}

func TestPaymentOperationsNotifyActivityFailureRequestsSafeRetry(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	ctx := context.Background()
	if _, err := f.db.DB.Exec(ctx, `CREATE FUNCTION reject_notify_activity() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source='notify' THEN RAISE EXCEPTION 'test rejection'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_notify_activity BEFORE INSERT ON payment_activity FOR EACH ROW EXECUTE FUNCTION reject_notify_activity()`); err != nil {
		t.Fatal(err)
	}
	n := payments.Notification{PaymentID: p.ID, TransactionID: "activity-retry-transaction", Amount: o.Price, Currency: o.Currency, Paid: true}
	if code, _ := paymentCallback(t, f, "alipay", n, true); code != 500 {
		t.Fatal("unrecorded completion acknowledged", code)
	}
	if _, err := f.db.DB.Exec(ctx, `DROP TRIGGER reject_notify_activity ON payment_activity`); err != nil {
		t.Fatal(err)
	}
	if code, _ := paymentCallback(t, f, "alipay", n, true); code != 200 {
		t.Fatal("safe retry rejected", code)
	}
	if a, b, c := paymentCounts(t, f, o.ID); a != 1 || b != 1 || c != 1 {
		t.Fatal("retry duplicated fulfillment", a, b, c)
	}
}

func TestPaymentOperationsInvalidNotificationSamplesAreBounded(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	n := payments.Notification{PaymentID: p.ID, TransactionID: "actual-provider-transaction", Amount: o.Price, Currency: o.Currency, Paid: true}
	for i := 0; i < 14; i++ {
		if code, _ := paymentCallback(t, f, "alipay", n, false); code != 400 {
			t.Fatal("invalid notification was not rejected", code)
		}
	}
	var count int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM payment_activity WHERE source='notify' AND outcome='verification_error'`).Scan(&count); err != nil || count != 10 {
		t.Fatal("invalid notification logging was not bounded", err, count)
	}
	// A legitimate notification still succeeds after exhausting the diagnostic
	// budget; failed proofs cannot suppress the actual financial callback.
	if code, _ := paymentCallback(t, f, "alipay", n, true); code != 200 {
		t.Fatal("invalid sample budget suppressed valid payment", code)
	}
}
