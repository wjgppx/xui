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
)

func TestQueuedPlanCreationCannotOutliveStaffSession(t *testing.T) {
	for _, item := range []struct {
		name, mutation, code string
		status               int
	}{
		{"account generation", `WITH account AS (UPDATE users SET security_version=security_version+1 WHERE id=$1) DELETE FROM sessions WHERE user_id=$1`, "", 401},
		{"logout", `DELETE FROM sessions WHERE user_id=$1`, "", 401},
		{"stepup expiry", `UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, "mfa_required", 403},
	} {
		t.Run(item.name, func(t *testing.T) {
			key := bytes.Repeat([]byte{57}, 32)
			f := mfaHTTPSetup(t, true, key)
			actor, cookie := staffAccount(t, f, "admin")
			_, _, cookie = mfaHTTPEnroll(t, f, cookie)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cfg := f.db.DB.Config()
			trace := "plan-session-" + store.ID()
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
			// A table lock allows authentication to finish and holds only the
			// INSERT, so the test observes actual queued business work.
			if _, err = lock.Exec(ctx, `LOCK TABLE plans IN SHARE MODE`); err != nil {
				t.Fatal(err)
			}
			type result struct {
				status int
				body   []byte
				err    error
			}
			finished := make(chan result, 1)
			go func() {
				req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/admin/plans", strings.NewReader(`{"name":"queued plan","description":"must not commit after revocation","price_minor":100,"currency":"CNY","duration_days":30,"quota_bytes":1000}`))
				if err != nil {
					finished <- result{err: err}
					return
				}
				req.Header.Set("Origin", origin)
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					finished <- result{err: err}
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				finished <- result{response.StatusCode, body, err}
			}()
			for {
				var waiting bool
				if err = f.db.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%INSERT INTO plans%')`, trace).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case got := <-finished:
					t.Fatalf("plan request did not reach business lock: status=%d err=%v", got.status, got.err)
				case <-ctx.Done():
					t.Fatal("plan request did not reach business lock")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if _, err = f.db.DB.Exec(ctx, item.mutation, actor.ID); err != nil {
				t.Fatal(err)
			}
			if err = lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var got result
			select {
			case got = <-finished:
			case <-ctx.Done():
				t.Fatal("queued plan request did not return")
			}
			if got.err != nil || got.status != item.status || (item.code != "" && !bytes.Contains(got.body, []byte(`"code":"`+item.code+`"`))) {
				t.Fatal("stale staff session created plan", got.status, got.err)
			}
			var plans, audits int
			if err = f.db.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM plans WHERE name='queued plan'),(SELECT count(*) FROM audit_events WHERE actor_id=$1 AND action='plan.created')`, actor.ID).Scan(&plans, &audits); err != nil {
				t.Fatal(err)
			}
			if plans != 0 || audits != 0 {
				t.Fatal("rejected plan write or audit escaped rollback")
			}
		})
	}
}
