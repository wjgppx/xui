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

// A queued transaction may finish its business writes after the browser session
// has been revoked. Nothing, including its audit/outbox records, may commit.
func TestAccessQueuedWritesRecheckSessionAndRollBackBusinessEffects(t *testing.T) {
	for _, item := range []struct {
		name, operation, mutation, reason string
		status                            int
		alreadyRevoked                    bool
	}{
		{"group update after logout", "group", `DELETE FROM sessions WHERE user_id=$1`, "", 401, false},
		{"plan authorization after password generation change", "plan", `UPDATE users SET security_version=security_version+1 WHERE id=$1`, "", 401, false},
		{"event retry after step-up expires", "event", `UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, "mfa_required", 403, false},
		{"entitlement revoke after logout", "entitlement", `DELETE FROM sessions WHERE user_id=$1`, "", 401, false},
		{"idempotent revoke after logout", "entitlement", `DELETE FROM sessions WHERE user_id=$1`, "", 401, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			key := bytes.Repeat([]byte{67}, 32)
			f := mfaHTTPSetup(t, true, key)
			actor, cookie := staffAccount(t, f, "admin")
			_, _, cookie = mfaHTTPEnroll(t, f, cookie)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			group, event, node := store.ID(), store.ID(), store.ID()
			if _, err := f.db.DB.Exec(ctx, `INSERT INTO nodes(id,name,enrollment_hash,enrollment_expires_at) VALUES($1,'session fixture',$2,now()+interval '1 hour')`, node, store.ID()+store.ID()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB.Exec(ctx, `INSERT INTO node_groups(id,name) VALUES($1,'original')`, group); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB.Exec(ctx, `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2)`, group, node); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB.Exec(ctx, `INSERT INTO plan_node_groups(plan_id,group_id) VALUES('explore',$1)`, group); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.DB.Exec(ctx, `INSERT INTO outbox_events(id,kind,aggregate_id,payload,attempts,last_error,dead_lettered_at) VALUES($1,'fixture',$1,'{}',5,'unsupported_event',now())`, event); err != nil {
				t.Fatal(err)
			}
			customer := testutil.User(t, f.db)
			order, err := f.db.CreateOrder(ctx, customer, "explore")
			if err != nil {
				t.Fatal(err)
			}
			if err = f.db.Settle(ctx, "access-session-test", "paid", order.ID, customer, order.Price, order.Currency); err != nil {
				t.Fatal(err)
			}
			var entitlement string
			if err = f.db.DB.QueryRow(ctx, `SELECT id FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&entitlement); err != nil {
				t.Fatal(err)
			}
			if item.alreadyRevoked {
				if _, err = f.db.DB.Exec(ctx, `UPDATE entitlement_periods SET revoked_at=now() WHERE id=$1`, entitlement); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := func() string {
				t.Helper()
				var state string
				err := f.db.DB.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY id),'[]') FROM node_groups g),
 (SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY group_id,node_id),'[]') FROM node_group_members m),
 (SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY id),'[]') FROM plans p),
 (SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY plan_id,group_id),'[]') FROM plan_node_groups g),
 (SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY id),'[]') FROM entitlement_periods e),
 (SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY id),'[]') FROM outbox_events e),
 (SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]') FROM audit_events a))::text`).Scan(&state)
				if err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := snapshot()
			path, body, lockQuery, lockID := "", "{}", "", ""
			switch item.operation {
			case "group":
				path, body = "/api/v1/admin/node-groups/"+group, `{"name":"changed","enabled":false,"node_ids":[],"revision":1}`
				lockQuery, lockID = `SELECT id FROM node_groups WHERE id=$1 FOR UPDATE`, group
			case "plan":
				path, body = "/api/v1/admin/plans/explore/node-groups", `{"group_ids":[],"revision":1}`
				lockQuery, lockID = `SELECT id FROM plans WHERE id=$1 FOR UPDATE`, "explore"
			case "event":
				path = "/api/v1/admin/events/" + event + "/retry"
				lockQuery, lockID = `SELECT id FROM outbox_events WHERE id=$1 FOR UPDATE`, event
			case "entitlement":
				path = "/api/v1/admin/entitlements/" + entitlement + "/revoke"
				lockQuery, lockID = `SELECT id FROM entitlement_periods WHERE id=$1 FOR UPDATE`, entitlement
			}
			config := f.db.DB.Config()
			trace := "access-session-" + store.ID()
			config.ConnConfig.RuntimeParams["application_name"] = trace
			pool, err := pgxpool.NewWithConfig(ctx, config)
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
			var locked string
			if err = lock.QueryRow(ctx, lockQuery, lockID).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			type response struct {
				status int
				body   []byte
				err    error
			}
			result := make(chan response, 1)
			go func() {
				req, err := http.NewRequestWithContext(ctx, "POST", server.URL+path, strings.NewReader(body))
				if err != nil {
					result <- response{err: err}
					return
				}
				req.Header.Set("Origin", origin)
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				got, err := http.DefaultClient.Do(req)
				if err != nil {
					result <- response{err: err}
					return
				}
				defer got.Body.Close()
				data, err := io.ReadAll(got.Body)
				result <- response{got.StatusCode, data, err}
			}()
			for {
				var waiting bool
				if err = f.db.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')`, trace).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case got := <-result:
					t.Fatalf("request did not wait for its business row: status=%d err=%v", got.status, got.err)
				case <-ctx.Done():
					t.Fatal("request did not reach its business row lock")
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
				t.Fatal("rejected queued request did not return")
			}
			if got.err != nil || got.status != item.status || (item.reason != "" && !bytes.Contains(got.body, []byte(`"code":"`+item.reason+`"`))) {
				t.Fatalf("queued access write kept revoked authority: status=%d err=%v", got.status, got.err)
			}
			if after := snapshot(); after != before {
				t.Fatal("rejected access transaction changed business state, outbox or audit")
			}
		})
	}
}
