package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/payments"
)

const billSchedulePath = "/api/v1/admin/bill-schedules"

type billScheduleHTTPItem struct {
	ID               string  `json:"id"`
	Provider         string  `json:"provider"`
	Kind             string  `json:"kind"`
	Enabled          bool    `json:"enabled"`
	RunAt            string  `json:"run_at"`
	Timezone         string  `json:"timezone"`
	Revision         int64   `json:"revision"`
	State            string  `json:"state"`
	NextBillDate     *string `json:"next_bill_date"`
	LastEnqueuedDate *string `json:"last_enqueued_date"`
	LastJobID        *string `json:"last_job_id"`
	AuthorizedBy     *string `json:"authorized_by"`
}

func billScheduleHTTPBody(enabled bool, revision int64) string {
	return billScheduleHTTPEncode(map[string]any{"provider": "wechat", "kind": "payment", "enabled": enabled, "run_at": "10:00", "revision": revision})
}

func billScheduleHTTPEncode(body map[string]any) string {
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func TestBillSchedulesHTTPRestrictsManagementToFinanceAndAdmins(t *testing.T) {
	for _, role := range []string{"admin", "finance", "support", "operator", "user"} {
		t.Run(role, func(t *testing.T) {
			provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
			f := nativeBillHTTPSetup(t, true, provider)
			_, cookie := staffAccount(t, f, role)
			want := http.StatusForbidden
			if role == "admin" || role == "finance" {
				want = http.StatusOK
			}
			for _, endpoint := range []struct{ method, body string }{{"GET", ""}, {"PUT", billScheduleHTTPBody(true, 1)}} {
				code, reply, _ := request(t, f, endpoint.method, billSchedulePath, endpoint.body, cookie)
				if code != want {
					t.Fatal(endpoint.method, code, want, string(reply))
				}
			}
			if provider.calls.Load() != 0 {
				t.Fatal("schedule management contacted the merchant")
			}
		})
	}
}

func TestBillSchedulesHTTPUsesServerAuthorityWithoutDownloadingOrExposingSecrets(t *testing.T) {
	provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
	f := nativeBillHTTPSetup(t, true, provider)
	actor, cookie := staffAccount(t, f, "finance")
	code, reply, _ := request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(true, 1), cookie)
	var item billScheduleHTTPItem
	if code != http.StatusOK || json.Unmarshal(reply, &item) != nil || !item.Enabled || item.State != "active" || item.Revision != 2 || item.RunAt != "10:00" || item.Timezone != "Asia/Shanghai" || item.AuthorizedBy == nil || *item.AuthorizedBy != actor.ID || item.NextBillDate == nil || item.LastEnqueuedDate != nil || item.LastJobID != nil {
		t.Fatal("invalid scheduled authority", code, string(reply))
	}
	yesterday := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).AddDate(0, 0, -1).Format("2006-01-02")
	if *item.NextBillDate != yesterday {
		t.Fatal("schedule did not start at yesterday in Shanghai", *item.NextBillDate, yesterday)
	}
	code, reply, _ = request(t, f, "GET", billSchedulePath, "", cookie)
	var list struct {
		Items []billScheduleHTTPItem `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(reply, &list) != nil || len(list.Items) != 4 {
		t.Fatal("missing seeded schedule list", code, string(reply))
	}
	for _, private := range []string{"merchant_fingerprint", "actor_version", "security_version", "authorization_generation", "private_key", "download_url"} {
		if bytes.Contains(reply, []byte(private)) {
			t.Fatal("schedule response exposed private authority", private)
		}
	}
	var jobs int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM bill_download_jobs`).Scan(&jobs); err != nil || jobs != 0 || provider.calls.Load() != 0 {
		t.Fatal("saving schedule performed a download", jobs, provider.calls.Load(), err)
	}
	code, reply, _ = request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(false, 1), cookie)
	if code != http.StatusConflict || !bytes.Contains(reply, []byte("刷新")) {
		t.Fatal("stale revision silently replaced authorization", code, string(reply))
	}
}

func TestBillSchedulesHTTPGlobalGateStillAllowsPausingWithoutAConfiguredMerchant(t *testing.T) {
	provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
	f := nativeBillHTTPSetup(t, true, provider)
	_, cookie := staffAccount(t, f, "finance")
	code, reply, _ := request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(true, 1), cookie)
	if code != http.StatusOK {
		t.Fatal(code, string(reply))
	}
	server := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4, BillDownloadsEnabled: false}).Handler())
	t.Cleanup(server.Close)
	disabled := fixture{f.db, server}
	code, reply, _ = request(t, disabled, "GET", billSchedulePath, "", cookie)
	if code != http.StatusOK || !bytes.Contains(reply, []byte(`"enabled":true`)) {
		t.Fatal("gate hid saved configuration", code, string(reply))
	}
	code, reply, _ = request(t, disabled, "PUT", billSchedulePath, billScheduleHTTPBody(false, 2), cookie)
	var item billScheduleHTTPItem
	if code != http.StatusOK || json.Unmarshal(reply, &item) != nil || item.Enabled || item.State != "disabled" || item.Revision != 3 {
		t.Fatal("gate prevented revoking schedule authority", code, string(reply))
	}
	code, reply, _ = request(t, disabled, "PUT", billSchedulePath, billScheduleHTTPBody(true, 3), cookie)
	if code != http.StatusServiceUnavailable {
		t.Fatal("disabled global gate allowed new authority", code, string(reply))
	}
	if provider.calls.Load() != 0 {
		t.Fatal("configuration contacted merchant")
	}
}

func TestBillSchedulesHTTPRejectsInvalidConfigurationAndCallerControlledAuthority(t *testing.T) {
	base := map[string]any{"provider": "wechat", "kind": "payment", "enabled": true, "run_at": "10:00", "revision": 1}
	bad := func(key string, value any) string {
		body := make(map[string]any, len(base)+1)
		for k, v := range base {
			body[k] = v
		}
		body[key] = value
		return billScheduleHTTPEncode(body)
	}
	for name, body := range map[string]string{
		"unsupported_provider": bad("provider", "epay_alipay"),
		"unsupported_kind":     bad("kind", "transfer"),
		"invalid_time":         bad("run_at", "24:00"),
		"invalid_minutes":      bad("run_at", "10:60"),
		"short_hour":           bad("run_at", "9:00"),
		"timezone_in_time":     bad("run_at", "10:00+08:00"),
		"zero_revision":        bad("revision", 0),
		"negative_revision":    bad("revision", -1),
		"fractional_revision":  bad("revision", 1.5),
		"null_enabled":         bad("enabled", nil),
		"string_enabled":       bad("enabled", "true"),
		"missing_enabled":      `{"provider":"wechat","kind":"payment","run_at":"10:00","revision":1}`,
		"caller_fingerprint":   bad("merchant_fingerprint", strings.Repeat("b", 64)),
		"caller_owner":         bad("authorized_by", "private-fixture-owner"),
		"caller_version":       bad("actor_version", 2),
		"caller_cursor":        bad("next_bill_date", "2026-01-01"),
		"caller_timezone":      bad("timezone", "UTC"),
		"caller_url":           bad("download_url", "https://127.0.0.1/private-fixture-url"),
		"multiple_objects":     billScheduleHTTPEncode(base) + `{}`,
		"oversized":            bad("download_url", strings.Repeat("x", 65537)),
		"null":                 "null",
	} {
		t.Run(name, func(t *testing.T) {
			provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
			f := nativeBillHTTPSetup(t, true, provider)
			_, cookie := staffAccount(t, f, "finance")
			code, reply, _ := request(t, f, "PUT", billSchedulePath, body, cookie)
			if code != http.StatusBadRequest || bytes.Contains(reply, []byte("private-fixture")) {
				t.Fatal(code, string(reply))
			}
			items, err := f.db.ListBillSchedules(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Enabled || item.Revision != 1 {
					t.Fatal("invalid request changed schedule", item)
				}
			}
			if provider.calls.Load() != 0 {
				t.Fatal("invalid schedule contacted merchant")
			}
		})
	}
}

func TestBillSchedulesHTTPRequiresConfiguredBillCapabilityOnlyForEnabling(t *testing.T) {
	for name, test := range map[string]struct {
		providers []payments.Provider
		want      int
	}{
		"missing_provider":   {nil, http.StatusServiceUnavailable},
		"no_bill_capability": {[]payments.Provider{&paymentFake{id: "wechat"}}, http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			f := nativeBillHTTPSetup(t, true, test.providers...)
			_, cookie := staffAccount(t, f, "finance")
			code, reply, _ := request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(true, 1), cookie)
			if code != test.want {
				t.Fatal(code, test.want, string(reply))
			}
			code, reply, _ = request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(false, 1), cookie)
			if code != http.StatusOK {
				t.Fatal("merchant configuration blocked pause", code, string(reply))
			}
		})
	}
}

func TestBillSchedulesHTTPProtectsOriginJSONQueryAndVerifiedSessions(t *testing.T) {
	f := nativeBillHTTPSetup(t, true)
	_, cookie := staffAccount(t, f, "finance")
	for _, endpoint := range []struct{ method, path, body string }{{"GET", billSchedulePath + "?provider=wechat", ""}, {"PUT", billSchedulePath + "?revision=1", billScheduleHTTPBody(false, 1)}} {
		code, reply, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
		if code != http.StatusBadRequest {
			t.Fatal(code, string(reply))
		}
	}
	for _, test := range []struct {
		origin, contentType string
		cookie              bool
		want                int
	}{
		{"", "application/json", true, http.StatusForbidden},
		{"https://untrusted.example.test", "application/json", true, http.StatusForbidden},
		{origin, "text/plain", true, http.StatusUnsupportedMediaType},
		{origin, "application/json", false, http.StatusUnauthorized},
	} {
		req, err := http.NewRequest("PUT", f.server.URL+billSchedulePath, strings.NewReader(billScheduleHTTPBody(false, 1)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Content-Type", test.contentType)
		if test.cookie {
			req.AddCookie(cookie)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		reply, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != test.want {
			t.Fatal(response.StatusCode, string(reply), readErr)
		}
	}
	server := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4, RequireVerified: true}).Handler())
	t.Cleanup(server.Close)
	verifiedOnly := fixture{f.db, server}
	for _, endpoint := range []struct{ method, body string }{{"GET", ""}, {"PUT", billScheduleHTTPBody(false, 1)}} {
		code, reply, _ := request(t, verifiedOnly, endpoint.method, billSchedulePath, endpoint.body, cookie)
		if code != http.StatusForbidden || !bytes.Contains(reply, []byte("邮箱验证")) {
			t.Fatal("unverified account configured automatic access", endpoint.method, code, string(reply))
		}
	}
}

func TestBillSchedulesHTTPIndependentWriteLimitAndStorageErrorsStayPrivate(t *testing.T) {
	t.Run("rate_limit", func(t *testing.T) {
		f := nativeBillHTTPSetup(t, true)
		_, cookie := staffAccount(t, f, "finance")
		for revision := int64(1); revision <= 5; revision++ {
			code, reply, _ := request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(false, revision), cookie)
			if code != http.StatusOK {
				t.Fatal(revision, code, string(reply))
			}
		}
		code, reply, _ := request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(false, 6), cookie)
		if code != http.StatusTooManyRequests {
			t.Fatal(code, string(reply))
		}
		code, reply, _ = request(t, f, "POST", "/api/v1/admin/statements/native", accountBody(nativeHTTPInput(nativeHTTPWechatCSV(t, "external-payment-fixture"))), cookie)
		if code != http.StatusOK {
			t.Fatal("schedule rate limiter exhausted unrelated import budget", code, string(reply))
		}
	})
	t.Run("limiter_failure", func(t *testing.T) {
		f := nativeBillHTTPSetup(t, true)
		_, cookie := staffAccount(t, f, "finance")
		_, err := f.db.DB.Exec(context.Background(), `CREATE FUNCTION reject_schedule_rate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private fixture failure'; END $$; CREATE TRIGGER reject_schedule_rate BEFORE INSERT ON rate_limit_buckets FOR EACH ROW EXECUTE FUNCTION reject_schedule_rate()`)
		if err != nil {
			t.Fatal(err)
		}
		code, reply, _ := request(t, f, "PUT", billSchedulePath, billScheduleHTTPBody(false, 1), cookie)
		if code != http.StatusServiceUnavailable || bytes.Contains(reply, []byte("private fixture failure")) {
			t.Fatal(code, string(reply))
		}
	})
	t.Run("storage_failure", func(t *testing.T) {
		f := nativeBillHTTPSetup(t, true)
		_, cookie := staffAccount(t, f, "finance")
		_, err := f.db.DB.Exec(context.Background(), `ALTER TABLE bill_download_schedules RENAME TO unavailable_schedule_fixture`)
		if err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []struct{ method, body string }{{"GET", ""}, {"PUT", billScheduleHTTPBody(false, 1)}} {
			code, reply, _ := request(t, f, endpoint.method, billSchedulePath, endpoint.body, cookie)
			if code != http.StatusServiceUnavailable || bytes.Contains(reply, []byte("bill_download_schedules")) || bytes.Contains(reply, []byte("unavailable_schedule_fixture")) {
				t.Fatal("storage error exposed database detail", endpoint.method, code, string(reply))
			}
		}
	})
}
