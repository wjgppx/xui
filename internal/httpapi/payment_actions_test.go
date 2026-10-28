package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func actionSetup(t *testing.T, enabled bool) fixture {
	t.Helper()
	s := testutil.Database(t)
	registry, err := payments.New(&paymentFake{id: "alipay"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.New(s, httpapi.Config{Origin: origin, PasswordCost: 4, Payments: registry, PaymentActionsEnabled: enabled}).Handler())
	t.Cleanup(server.Close)
	return fixture{s, server}
}
func actionPaid(t *testing.T, f fixture) store.Payment {
	t.Helper()
	u, _ := register(t, f)
	o := paymentOrder(t, f, u)
	p, err := f.db.CreatePayment(context.Background(), u.ID, o.ID, "alipay")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.SettlePayment(context.Background(), p.ID, p.Provider, "transaction-"+p.ID, o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestActionHTTPGateKeepsDraftButPreventsFundOperations(t *testing.T) {
	f := actionSetup(t, false)
	_, admin := staffAccount(t, f, "admin")
	p := actionPaid(t, f)
	code, b, _ := request(t, f, "POST", "/api/v1/admin/payments/"+p.ID+"/refund", `{"reason":"全额退款"}`, admin)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var r store.Refund
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	code, _, _ = request(t, f, "POST", "/api/v1/admin/refunds/"+r.ID+"/approve", `{"current_password":"sufficiently-long-password"}`, admin)
	if code != 503 {
		t.Fatal(code)
	}
	code, _, _ = request(t, f, "POST", "/api/v1/admin/payments/"+p.ID+"/close", `{"reason":"关单","current_password":"sufficiently-long-password"}`, admin)
	if code != 503 {
		t.Fatal(code)
	}
	var jobs int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM payment_action_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal("disabled action queued", jobs, err)
	}
	code, b, _ = request(t, f, "GET", "/api/v1/config", "", nil)
	var config map[string]bool
	_ = json.Unmarshal(b, &config)
	if code != 200 || config["payment_actions_enabled"] {
		t.Fatal("disabled gate not advertised")
	}
}
func TestActionHTTPApprovalRequiresAdminAndFreshPassword(t *testing.T) {
	f := actionSetup(t, true)
	_, admin := staffAccount(t, f, "admin")
	_, finance := staffAccount(t, f, "finance")
	p := actionPaid(t, f)
	code, b, _ := request(t, f, "POST", "/api/v1/admin/payments/"+p.ID+"/refund", `{"reason":"全额退款"}`, finance)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var r store.Refund
	_ = json.Unmarshal(b, &r)
	path := "/api/v1/admin/refunds/" + r.ID + "/approve"
	code, _, _ = request(t, f, "POST", path, `{"current_password":"sufficiently-long-password"}`, finance)
	if code != 403 {
		t.Fatal("finance approved", code)
	}
	code, _, _ = request(t, f, "POST", path, `{"current_password":"incorrect-password"}`, admin)
	if code != 401 {
		t.Fatal("bad password approved", code)
	}
	code, b, _ = request(t, f, "POST", path, `{"current_password":"sufficiently-long-password"}`, admin)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	code, b, _ = request(t, f, "GET", "/api/v1/admin/refunds", "", finance)
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var list struct {
		Items []store.RefundOverview `json:"items"`
	}
	_ = json.Unmarshal(b, &list)
	if len(list.Items) != 1 || list.Items[0].Status != "queued" {
		t.Fatal(string(b))
	}
}
func TestActionHTTPRejectsEditableAmountsUnknownFiltersAndUnauthorizedRoles(t *testing.T) {
	f := actionSetup(t, true)
	_, admin := staffAccount(t, f, "admin")
	_, support := staffAccount(t, f, "support")
	_, user := register(t, f)
	p := actionPaid(t, f)
	path := "/api/v1/admin/payments/" + p.ID + "/refund"
	for _, body := range []string{`{"reason":"test","amount_minor":1}`, `{"reason":""}`, `{"reason":"bad\nreason"}`} {
		code, b, _ := request(t, f, "POST", path, body, admin)
		if code != 400 {
			t.Fatal(code, string(b))
		}
	}
	for _, cookie := range []*http.Cookie{support, user} {
		code, _, _ := request(t, f, "GET", "/api/v1/admin/refunds", "", cookie)
		if code != 403 {
			t.Fatal(code)
		}
		code, _, _ = request(t, f, "POST", path, `{"reason":"refund"}`, cookie)
		if code != 403 {
			t.Fatal(code)
		}
	}
	code, _, _ := request(t, f, "GET", "/api/v1/admin/refunds?status=all", "", admin)
	if code != 400 {
		t.Fatal(code)
	}
}
func TestActionHTTPCloseReauthAndQueueFence(t *testing.T) {
	f := actionSetup(t, true)
	_, finance := staffAccount(t, f, "finance")
	u, _ := register(t, f)
	o := paymentOrder(t, f, u)
	p, err := f.db.CreatePayment(context.Background(), u.ID, o.ID, "alipay")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/payments/" + p.ID + "/close"
	code, _, _ := request(t, f, "POST", path, `{"reason":"不再付款","current_password":"wrong-password"}`, finance)
	if code != 401 {
		t.Fatal(code)
	}
	code, b, _ := request(t, f, "POST", path, `{"reason":"不再付款","current_password":"sufficiently-long-password"}`, finance)
	if code != 202 {
		t.Fatal(code, string(b))
	}
	got, err := f.db.PaymentOrder(context.Background(), u.ID, o.ID)
	if err != nil || got.Status != "closing" {
		t.Fatal(got, err)
	}
}
