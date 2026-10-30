package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func TestStaffQueuedRoleRequestCannotOutliveSessionOrMFAInvalidation(t *testing.T) {
	for _, item := range []struct {
		name, mutation, code string
		status               int
	}{
		{"password or MFA reset", `WITH account AS (UPDATE users SET security_version=security_version+1 WHERE id=$1) DELETE FROM sessions WHERE user_id=$1`, "", 401},
		{"logout", `DELETE FROM sessions WHERE user_id=$1`, "", 401},
		{"login expiry", `UPDATE sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, "", 401},
		{"stepup expiry", `UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, "mfa_required", 403},
	} {
		t.Run(item.name, func(t *testing.T) {
			key := bytes.Repeat([]byte{53}, 32)
			f := mfaHTTPSetup(t, true, key)
			actor, cookie := staffAccount(t, f, "admin")
			_, _, cookie = mfaHTTPEnroll(t, f, cookie)
			target := testutil.User(t, f.db)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			// Give only this HTTP server a recognizable connection identity. The
			// observer checks its actual database lock wait rather than sleeping.
			cfg := f.db.DB.Config()
			trace := "role-session-" + store.ID()
			cfg.ConnConfig.RuntimeParams["application_name"] = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			policy := *f.db
			policy.DB = pool
			server := httptest.NewServer(httpapi.New(&policy, httpapi.Config{Origin: origin, Development: true, PasswordCost: 4, MFAKey: key, RequireStaffMFA: true}).Handler())
			t.Cleanup(server.Close)
			lock, err := f.db.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(context.Background())
			if _, err = lock.Exec(ctx, `SELECT pg_advisory_xact_lock(738912409)`); err != nil {
				t.Fatal(err)
			}
			type response struct {
				status int
				body   []byte
				err    error
			}
			result := make(chan response, 1)
			go func() {
				req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/admin/users/"+target+"/role", strings.NewReader(`{"role":"admin"}`))
				if err != nil {
					result <- response{err: err}
					return
				}
				req.Header.Set("Origin", origin)
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				r, err := http.DefaultClient.Do(req)
				if err != nil {
					result <- response{err: err}
					return
				}
				defer r.Body.Close()
				body, err := io.ReadAll(r.Body)
				result <- response{r.StatusCode, body, err}
			}()
			for {
				var waiting bool
				if err := f.db.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock(738912409)%')`, trace).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case got := <-result:
					t.Fatalf("role request did not reach transaction lock: %d %v", got.status, got.err)
				case <-ctx.Done():
					t.Fatal("role request did not reach lock")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if _, err = f.db.DB.Exec(ctx, item.mutation, actor.ID); err != nil {
				t.Fatal(err)
			}
			if err = lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var got response
			select {
			case got = <-result:
			case <-ctx.Done():
				t.Fatal("queued request did not return")
			}
			if got.err != nil || got.status != item.status || (item.code != "" && !bytes.Contains(got.body, []byte(`"code":"`+item.code+`"`))) {
				t.Fatal("revoked or expired actor granted role", got.status, got.err)
			}
			var role string
			var audits int
			if err := f.db.DB.QueryRow(ctx, `SELECT role,(SELECT count(*) FROM audit_events WHERE resource_id=$1) FROM users WHERE id=$1`, target).Scan(&role, &audits); err != nil {
				t.Fatal(err)
			}
			if role != "user" || audits != 0 {
				t.Fatal("rejected queued operation changed target or audit")
			}
		})
	}
}
