package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type paymentFake struct {
	id          string
	mu          sync.Mutex
	requests    []payments.Request
	createError error
	query       func(payments.Request) (payments.Notification, error)
}

func (p *paymentFake) ID() string   { return p.id }
func (p *paymentFake) Name() string { return "测试支付" }
func (p *paymentFake) Create(_ context.Context, r payments.Request) (payments.Checkout, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, r)
	if p.createError != nil {
		return payments.Checkout{}, p.createError
	}
	return payments.Checkout{Kind: "redirect", URL: "https://pay.example.test/checkout?out_trade_no=" + r.PaymentID}, nil
}
func (p *paymentFake) Verify(r *http.Request) (payments.Notification, error) {
	if r.Header.Get("X-Test-Signature") != "valid" {
		return payments.Notification{}, payments.ErrVerification
	}
	var n payments.Notification
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		return n, payments.ErrVerification
	}
	return n, nil
}
func (p *paymentFake) Query(_ context.Context, r payments.Request) (payments.Notification, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.query == nil {
		return payments.Notification{}, payments.ErrNotPaid
	}
	return p.query(r)
}
func paymentSetup(t *testing.T, verified bool, providers ...payments.Provider) fixture {
	t.Helper()
	s := testutil.Database(t)
	registry, err := payments.New(providers...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.New(s, httpapi.Config{Origin: origin, Development: true, RequireVerified: verified, PasswordCost: 4, Payments: registry}).Handler())
	t.Cleanup(server.Close)
	return fixture{s, server}
}
func paymentOrder(t *testing.T, f fixture, u store.User) store.Order {
	t.Helper()
	o, err := f.db.CreateOrder(context.Background(), u.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func paymentCreate(t *testing.T, f fixture, o store.Order, c *http.Cookie, provider string) store.Payment {
	t.Helper()
	code, b, _ := request(t, f, "POST", "/api/v1/orders/"+o.ID+"/payment", accountBody(map[string]string{"provider": provider}), c)
	if code != 200 {
		t.Fatalf("create payment %d %s", code, b)
	}
	var p store.Payment
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func paymentCallback(t *testing.T, f fixture, provider string, n payments.Notification, signature bool) (int, string) {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("POST", f.server.URL+"/api/v1/payments/notify/"+provider, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	// Real providers do not send the browser Origin header.
	r.Header.Set("Content-Type", "application/json")
	if signature {
		r.Header.Set("X-Test-Signature", "valid")
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}
func paymentCounts(t *testing.T, f fixture, orderID string) (int, int, int) {
	t.Helper()
	var events, rights, outbox int
	err := f.db.DB.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM payment_events WHERE order_id=$1),(SELECT count(*) FROM entitlement_periods WHERE order_id=$1),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND kind='entitlement.granted')`, orderID).Scan(&events, &rights, &outbox)
	if err != nil {
		t.Fatal(err)
	}
	return events, rights, outbox
}

func TestPaymentMethodsOwnershipCheckoutSnapshotAndChannelPin(t *testing.T) {
	alipay := &paymentFake{id: "alipay"}
	wechat := &paymentFake{id: "wechat"}
	f := paymentSetup(t, false, alipay, wechat)
	u, c := register(t, f)
	_, other := register(t, f)
	o := paymentOrder(t, f, u)
	if code, b, _ := request(t, f, "GET", "/api/v1/payment-methods", "", nil); code != 200 || !strings.Contains(string(b), `"id":"alipay"`) || strings.Contains(string(b), "epay") {
		t.Fatalf("methods %d %s", code, b)
	}
	path := "/api/v1/orders/" + o.ID + "/payment"
	for _, tc := range []struct {
		cookie *http.Cookie
		want   int
	}{{nil, 401}, {other, 404}} {
		if code, b, _ := request(t, f, "POST", path, `{"provider":"alipay"}`, tc.cookie); code != tc.want {
			t.Fatalf("owner %d %s", code, b)
		}
	}
	if code, _, _ := request(t, f, "POST", path, `{"provider":"epay_alipay"}`, c); code != 404 {
		t.Fatal("disabled provider accepted", code)
	}
	p := paymentCreate(t, f, o, c, "alipay")
	second := paymentCreate(t, f, o, c, "alipay")
	if p.ID != second.ID || p.Status != "ready" || p.CheckoutURL != second.CheckoutURL || len(alipay.requests) != 1 {
		t.Fatal("checkout was not reused")
	}
	r := alipay.requests[0]
	if r.PaymentID != p.ID || r.OrderID != o.ID || r.Amount != o.Price || r.Currency != "CNY" || r.Name != o.PlanName || r.NotifyURL != origin+"/api/v1/payments/notify/alipay" || r.ReturnURL != origin+"/?payment_return=1" {
		t.Fatal("immutable order or configured origin not used")
	}
	if code, _, _ := request(t, f, "POST", path, `{"provider":"wechat"}`, c); code != 409 {
		t.Fatal("provider pin bypassed", code)
	}
	if code, _, _ := request(t, f, "GET", path, "", other); code != 404 {
		t.Fatal("other user saw payment", code)
	}
	if code, b, _ := request(t, f, "GET", path, "", c); code != 200 || !strings.Contains(string(b), p.ID) {
		t.Fatalf("own payment missing %d %s", code, b)
	}
}

func TestPaymentGatewayRetryRetainsMerchantIdentity(t *testing.T) {
	pv := &paymentFake{id: "alipay", createError: payments.ErrGateway}
	f := paymentSetup(t, false, pv)
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	if code, _, _ := request(t, f, "POST", "/api/v1/orders/"+o.ID+"/payment", `{"provider":"alipay"}`, c); code != 502 {
		t.Fatal(code)
	}
	p, err := f.db.Payment(context.Background(), u.ID, o.ID)
	if err != nil || p.Status != "pending" {
		t.Fatal("merchant identity not persisted before network", err)
	}
	pv.createError = nil
	ready := paymentCreate(t, f, o, c, "alipay")
	if p.ID != ready.ID || len(pv.requests) != 2 || pv.requests[0].PaymentID != pv.requests[1].PaymentID {
		t.Fatal("retry created another merchant identity")
	}
	if a, b, c := paymentCounts(t, f, o.ID); a+b+c != 0 {
		t.Fatal("checkout request granted rights")
	}
}

func TestPaymentCallbacksRejectForgeryAndReplayWithoutDuplicatingRights(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "alipay"}, &paymentFake{id: "wechat"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	n := payments.Notification{PaymentID: p.ID, TransactionID: "provider-transaction", Amount: o.Price, Currency: o.Currency, Paid: true}
	if code, _ := paymentCallback(t, f, "alipay", n, false); code != 400 {
		t.Fatal("forged callback accepted", code)
	}
	if code, _ := paymentCallback(t, f, "wechat", n, true); code != 400 {
		t.Fatal("cross-provider callback accepted", code)
	}
	wrong := n
	wrong.Amount++
	if code, _ := paymentCallback(t, f, "alipay", wrong, true); code != 400 {
		t.Fatal("wrong amount accepted", code)
	}
	wrong = n
	wrong.Currency = "USD"
	if code, _ := paymentCallback(t, f, "alipay", wrong, true); code != 400 {
		t.Fatal("wrong currency accepted", code)
	}
	pending := n
	pending.Paid = false
	if code, body := paymentCallback(t, f, "alipay", pending, true); code != 200 || body != "success" {
		t.Fatalf("pending acknowledgement %d %s", code, body)
	}
	if a, b, c := paymentCounts(t, f, o.ID); a+b+c != 0 {
		t.Fatal("untrusted callback granted rights")
	}
	for i := 0; i < 3; i++ {
		if code, body := paymentCallback(t, f, "alipay", n, true); code != 200 || body != "success" {
			t.Fatalf("verified callback %d %s", code, body)
		}
	}
	if a, b, c := paymentCounts(t, f, o.ID); a != 1 || b != 1 || c != 1 {
		t.Fatalf("duplicate rights: %d %d %d", a, b, c)
	}
	paid, err := f.db.Payment(context.Background(), u.ID, o.ID)
	if err != nil || paid.Status != "paid" || paid.TransactionID == nil || *paid.TransactionID != n.TransactionID {
		t.Fatal("intent was not settled atomically", err)
	}
	wrong = n
	wrong.TransactionID = "another-transaction"
	if code, _ := paymentCallback(t, f, "alipay", wrong, true); code != 400 {
		t.Fatal("second transaction accepted on paid order", code)
	}
	other := paymentOrder(t, f, u)
	otherPayment := paymentCreate(t, f, other, c, "alipay")
	wrong = n
	wrong.PaymentID = otherPayment.ID
	if code, _ := paymentCallback(t, f, "alipay", wrong, true); code != 400 {
		t.Fatal("transaction replayed against different order", code)
	}
	if a, b, c := paymentCounts(t, f, other.ID); a+b+c != 0 {
		t.Fatal("cross-order replay granted rights")
	}
}

func TestPaymentCallbackRollbackAndWechatAcknowledgement(t *testing.T) {
	f := paymentSetup(t, false, &paymentFake{id: "wechat"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "wechat")
	n := payments.Notification{PaymentID: p.ID, TransactionID: "wx-transaction", Amount: o.Price, Currency: o.Currency, Paid: true}
	ctx := context.Background()
	if _, err := f.db.DB.Exec(ctx, `CREATE FUNCTION fail_payment_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test payment failure'; END $$; CREATE TRIGGER payment_failure BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_payment_event()`); err != nil {
		t.Fatal(err)
	}
	if code, body := paymentCallback(t, f, "wechat", n, true); code != 500 || !strings.Contains(body, `"code":"FAIL"`) {
		t.Fatalf("failure ack %d %s", code, body)
	}
	if a, b, c := paymentCounts(t, f, o.ID); a+b+c != 0 {
		t.Fatal("failed callback partially committed")
	}
	unchanged, err := f.db.Payment(ctx, u.ID, o.ID)
	if err != nil || unchanged.Status != "ready" {
		t.Fatal("failed callback advanced intent", err)
	}
	if _, err = f.db.DB.Exec(ctx, `DROP TRIGGER payment_failure ON outbox_events`); err != nil {
		t.Fatal(err)
	}
	if code, body := paymentCallback(t, f, "wechat", n, true); code != 200 || !strings.Contains(body, `"code":"SUCCESS"`) {
		t.Fatalf("success ack %d %s", code, body)
	}
}

func TestPaymentSyncRequiresVerifiedExactQueryAndOwner(t *testing.T) {
	pv := &paymentFake{id: "alipay"}
	f := paymentSetup(t, false, pv)
	u, c := register(t, f)
	_, other := register(t, f)
	o := paymentOrder(t, f, u)
	p := paymentCreate(t, f, o, c, "alipay")
	path := "/api/v1/orders/" + o.ID + "/payment/sync"
	if code, _, _ := request(t, f, "POST", path, "{}", other); code != 404 {
		t.Fatal("other user queried payment", code)
	}
	pv.query = func(r payments.Request) (payments.Notification, error) {
		return payments.Notification{PaymentID: r.PaymentID, Amount: r.Amount, Currency: r.Currency, Paid: true, TransactionID: "query-transaction"}, payments.ErrVerification
	}
	if code, _, _ := request(t, f, "POST", path, "{}", c); code != 502 {
		t.Fatal("unverified query accepted", code)
	}
	for _, field := range []string{"identity", "amount", "currency"} {
		pv.query = func(r payments.Request) (payments.Notification, error) {
			n := payments.Notification{PaymentID: r.PaymentID, Amount: r.Amount, Currency: r.Currency, Paid: true, TransactionID: "query-transaction"}
			switch field {
			case "identity":
				n.PaymentID = store.ID()
			case "amount":
				n.Amount++
			case "currency":
				n.Currency = "USD"
			}
			return n, nil
		}
		if code, _, _ := request(t, f, "POST", path, "{}", c); code != 502 {
			t.Fatal("mismatched query accepted", field, code)
		}
	}
	if a, b, c := paymentCounts(t, f, o.ID); a+b+c != 0 {
		t.Fatal("unverified query granted rights")
	}
	pv.query = func(r payments.Request) (payments.Notification, error) {
		return payments.Notification{PaymentID: r.PaymentID, Amount: r.Amount, Currency: r.Currency, Paid: true, TransactionID: "query-transaction"}, nil
	}
	if code, b, _ := request(t, f, "POST", path, "{}", c); code != 200 || !strings.Contains(string(b), `"status":"paid"`) {
		t.Fatalf("query did not settle %d %s", code, b)
	}
	paid, err := f.db.Payment(context.Background(), u.ID, o.ID)
	if err != nil || paid.ID != p.ID || paid.Status != "paid" {
		t.Fatal(err)
	}
}

func TestPaymentProductionVerificationAndBrowserOrigin(t *testing.T) {
	f := paymentSetup(t, true, &paymentFake{id: "alipay"})
	u, c := register(t, f)
	o := paymentOrder(t, f, u)
	path := "/api/v1/orders/" + o.ID + "/payment"
	if code, _, _ := request(t, f, "POST", path, `{"provider":"alipay"}`, c); code != 403 {
		t.Fatal("unverified production checkout", code)
	}
	if _, err := f.db.Payment(context.Background(), u.ID, o.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unverified checkout persisted intent", err)
	}
	r, err := http.NewRequest("POST", f.server.URL+path, strings.NewReader(`{"provider":"alipay"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(c)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("browser origin validation bypassed", resp.StatusCode)
	}
	if code, _ := paymentCallback(t, f, "epay_alipay", payments.Notification{}, false); code != 404 {
		t.Fatal("disabled callback accepted", code)
	}
}
