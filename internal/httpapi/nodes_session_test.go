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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func TestQueuedNodeWritesCannotOutliveStaffAuthorization(t *testing.T) {
	for _, item := range []struct {
		name, operation, mutation, code string
		status                          int
	}{
		{"create after password or MFA reset", "create", `WITH account AS (UPDATE users SET security_version=security_version+1 WHERE id=$1) DELETE FROM sessions WHERE user_id=$1`, "", 401},
		{"rotate after logout", "rotate", `DELETE FROM sessions WHERE user_id=$1`, "", 401},
		{"revoke after role downgrade", "revoke", `UPDATE users SET role='user' WHERE id=$1`, "", 403},
		{"configure after second-factor expiry", "configure", `UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, "mfa_required", 403},
	} {
		t.Run(item.name, func(t *testing.T) {
			key := bytes.Repeat([]byte{67}, 32)
			f := mfaHTTPSetup(t, true, key)
			actor, cookie := staffAccount(t, f, "admin")
			_, _, cookie = mfaHTTPEnroll(t, f, cookie)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			nodeID := store.ID()
			if _, err := f.db.DB.Exec(ctx, `INSERT INTO nodes(id,name,enrollment_hash,enrollment_expires_at,agent_token_hash,enrolled_at,observed_revision,applied_revision,apply_status,core_instance_id) VALUES($1,'unchanged-node',$2,clock_timestamp()+interval '15 minutes',$3,clock_timestamp(),1,1,'applied','original-instance')`, nodeID, store.ID()+store.ID(), store.ID()+store.ID()); err != nil {
				t.Fatal(err)
			}
			path, body := "/api/v1/admin/nodes", `{"name":"queued-node"}`
			switch item.operation {
			case "rotate":
				path, body = "/api/v1/admin/nodes/"+nodeID+"/enrollment", `{}`
			case "revoke":
				path, body = "/api/v1/admin/nodes/"+nodeID+"/revoke", `{}`
			case "configure":
				user := testutil.User(t, f.db)
				configuration := map[string]any{"revision": 1, "bindings": []agentwire.BindingConfig{{ID: "vless-main", Protocol: "vless", Listen: "127.0.0.1", Port: 18443, ServerName: "node.example.test", Certificate: "node", Users: []string{user + "@example.test"}}}}
				data, err := json.Marshal(configuration)
				if err != nil {
					t.Fatal(err)
				}
				path, body = "/api/v1/admin/nodes/"+nodeID+"/configuration", string(data)
			}
			// Include all node fields and grants, so rejected rotation/configuration
			// cannot silently alter credentials, application state or authorizations.
			snapshot := func() string {
				t.Helper()
				var result string
				if err := f.db.DB.QueryRow(ctx, `SELECT jsonb_build_object('nodes',(SELECT COALESCE(jsonb_agg(to_jsonb(n) ORDER BY id),'[]'::jsonb) FROM nodes n),'grants',(SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY node_id,user_id),'[]'::jsonb) FROM node_grants g),'audits',(SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb) FROM audit_events a))::text`).Scan(&result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			before := snapshot()
			cfg := f.db.DB.Config()
			trace := "node-session-" + store.ID()
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
			if item.operation == "create" {
				_, err = lock.Exec(ctx, `LOCK TABLE nodes IN SHARE MODE`)
			} else {
				_, err = lock.Exec(ctx, `SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, nodeID)
			}
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				status int
				body   []byte
				err    error
			}
			completed := make(chan result, 1)
			go func() {
				req, err := http.NewRequestWithContext(ctx, "POST", server.URL+path, strings.NewReader(body))
				if err != nil {
					completed <- result{err: err}
					return
				}
				req.Header.Set("Origin", origin)
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					completed <- result{err: err}
					return
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				completed <- result{response.StatusCode, data, err}
			}()
			// Observe a real database lock wait for this isolated HTTP pool. This
			// establishes initial authentication completed before invalidation.
			for {
				var waiting bool
				if err := f.db.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE '%nodes%')`, trace).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case got := <-completed:
					t.Fatal("node request did not reach transaction lock", got.status, got.err)
				case <-ctx.Done():
					t.Fatal("node request did not reach transaction lock")
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
			case got = <-completed:
			case <-ctx.Done():
				t.Fatal("node request did not return after lock release")
			}
			if got.err != nil || got.status != item.status || (item.code != "" && !bytes.Contains(got.body, []byte(`"code":"`+item.code+`"`))) || bytes.Contains(got.body, []byte("enrollment_token")) {
				t.Fatal("stale node write was not rejected safely", got.status, got.err)
			}
			if snapshot() != before {
				t.Fatal("rejected node write changed nodes, grants or audits")
			}
		})
	}
}
