package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
)

func staffAccount(t *testing.T, f fixture, role string) (store.User, *http.Cookie) {
	t.Helper()
	u, c := register(t, f)
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE users SET role=$1 WHERE id=$2`, role, u.ID); err != nil {
		t.Fatal(err)
	}
	u.Role = role
	return u, c
}

func TestStaffPermissionBoundaries(t *testing.T) {
	f := setup(t, true)
	type endpoint struct {
		method, path, body string
		allowed            []string
		status             int
	}
	endpoints := []endpoint{
		{"GET", "/api/v1/admin/overview", "", []string{"admin", "finance"}, 200},
		{"GET", "/api/v1/admin/orders", "", []string{"admin", "finance", "support"}, 200},
		{"GET", "/api/v1/admin/nodes", "", []string{"admin", "operator"}, 200},
		{"GET", "/api/v1/admin/access", "", []string{"admin", "operator"}, 200},
		{"GET", "/api/v1/admin/events", "", []string{"admin", "operator"}, 200},
		{"GET", "/api/v1/admin/users", "", []string{"admin", "support"}, 200},
		{"POST", "/api/v1/admin/plans", `{"name":"权限测试套餐","description":"测试","price_minor":3000,"currency":"CNY","duration_days":30,"quota_bytes":1073741824}`, []string{"admin", "finance"}, 201},
		{"POST", "/api/v1/admin/nodes", `{"name":"权限测试节点"}`, []string{"admin", "operator"}, 201},
		{"POST", "/api/v1/admin/node-groups", `{"name":"权限测试组","node_ids":[],"enabled":true}`, []string{"admin", "operator"}, 201},
		{"POST", "/api/v1/admin/plans/explore/node-groups", `{"group_ids":[],"revision":1}`, []string{"admin", "operator"}, 200},
		{"POST", "/api/v1/admin/events/missing/retry", `{}`, []string{"admin", "operator"}, 404},
		{"POST", "/api/v1/admin/entitlements/missing/revoke", `{}`, []string{"admin"}, 404},
		{"POST", "/api/v1/admin/users/missing/role", `{"role":"support"}`, []string{"admin"}, 404},
	}
	for _, role := range []string{"user", "support", "finance", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			_, cookie := staffAccount(t, f, role)
			for _, ep := range endpoints {
				input := ep.body
				if ep.path == "/api/v1/admin/plans/explore/node-groups" {
					var revision int64
					if err := f.db.DB.QueryRow(context.Background(), `SELECT access_revision FROM plans WHERE id='explore'`).Scan(&revision); err != nil {
						t.Fatal(err)
					}
					encoded, err := json.Marshal(map[string]any{"group_ids": []string{}, "revision": revision})
					if err != nil {
						t.Fatal(err)
					}
					input = string(encoded)
				}
				status, body, _ := request(t, f, ep.method, ep.path, input, cookie)
				want := 403
				if slices.Contains(ep.allowed, role) {
					want = ep.status
				}
				if status != want {
					t.Errorf("%s %s: got %d want %d: %s", ep.method, ep.path, status, want, body)
				}
			}
		})
	}
}

func TestStaffUsersRejectAgentAndExposeOnlyAccountSummary(t *testing.T) {
	f := setup(t, true)
	_, enrollment, admin := adminNode(t, f)
	token := store.ID() + store.ID()
	status, body := machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: token})
	if status != 200 {
		t.Fatalf("enroll %d %s", status, body)
	}
	for _, path := range []string{"/api/v1/admin/users", "/api/v1/admin/orders", "/api/v1/admin/nodes"} {
		for _, bearer := range []string{"", token} {
			status, body = machine(t, f, "GET", path, bearer, nil)
			if status != 401 {
				t.Errorf("%s bearer=%t: got %d %s", path, bearer != "", status, body)
			}
		}
	}
	status, body, _ = request(t, f, "GET", "/api/v1/admin/users", "", admin)
	var users []map[string]json.RawMessage
	if status != 200 || json.Unmarshal(body, &users) != nil || len(users) == 0 {
		t.Fatalf("users %d %s", status, body)
	}
	allowed := []string{"id", "email", "role", "email_verified_at", "created_at"}
	for _, u := range users {
		if len(u) != len(allowed) {
			t.Fatalf("unexpected account fields: %v", u)
		}
		for key := range u {
			if !slices.Contains(allowed, key) {
				t.Errorf("sensitive or undocumented account field %q", key)
			}
		}
	}
}

func TestStaffRoleChangeInvalidatesAllSessionsAndProtectsAdminAccess(t *testing.T) {
	f := setup(t, true)
	adminUser, adminCookie := staffAccount(t, f, "admin")
	target, firstCookie := register(t, f)
	login, _ := json.Marshal(map[string]string{"email": target.Email, "password": "sufficiently-long-password"})
	status, body, cookies := request(t, f, "POST", "/api/v1/auth/login", string(login), nil)
	if status != 200 || len(cookies) != 1 {
		t.Fatalf("second login %d %s", status, body)
	}
	for _, role := range []string{"root", "", "ADMIN"} {
		input, _ := json.Marshal(map[string]string{"role": role})
		status, body, _ = request(t, f, "POST", "/api/v1/admin/users/"+target.ID+"/role", string(input), adminCookie)
		if status != 400 {
			t.Fatalf("invalid role accepted: %d %s", status, body)
		}
	}
	status, body, _ = request(t, f, "POST", "/api/v1/admin/users/"+target.ID+"/role", `{"role":"operator"}`, adminCookie)
	if status != 200 {
		t.Fatalf("role change %d %s", status, body)
	}
	for _, old := range []*http.Cookie{firstCookie, cookies[0]} {
		status, body, _ = request(t, f, "GET", "/api/v1/me", "", old)
		if status != 401 {
			t.Fatalf("old session survived role change: %d %s", status, body)
		}
	}
	var role string
	var version, sessions, audits int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT role,security_version,(SELECT count(*) FROM sessions WHERE user_id=$1),(SELECT count(*) FROM audit_events WHERE actor_id=$2 AND resource_id=$1 AND action='staff.role_changed') FROM users WHERE id=$1`, target.ID, adminUser.ID).Scan(&role, &version, &sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if role != "operator" || version != 1 || sessions != 0 || audits != 1 {
		t.Fatalf("role/security audit mismatch: %s %d %d %d", role, version, sessions, audits)
	}
	status, body, cookies = request(t, f, "POST", "/api/v1/auth/login", string(login), nil)
	var me store.User
	if status != 200 || len(cookies) != 1 || json.Unmarshal(body, &me) != nil || me.Role != "operator" || !slices.Contains(me.Permissions, "nodes.write") || slices.Contains(me.Permissions, "plans.write") {
		t.Fatalf("new session lacks new permissions: %d %s", status, body)
	}
	status, body, _ = request(t, f, "GET", "/api/v1/admin/nodes", "", cookies[0])
	if status != 200 {
		t.Fatalf("new operator session rejected: %d %s", status, body)
	}
	status, body, _ = request(t, f, "POST", "/api/v1/admin/users/"+adminUser.ID+"/role", `{"role":"user"}`, adminCookie)
	if status != 409 {
		t.Fatalf("last/self admin demotion accepted: %d %s", status, body)
	}
	// A second administrator still cannot remove their own access.
	staffAccount(t, f, "admin")
	status, body, _ = request(t, f, "POST", "/api/v1/admin/users/"+adminUser.ID+"/role", `{"role":"support"}`, adminCookie)
	if status != 409 {
		t.Fatalf("self demotion with another administrator accepted: %d %s", status, body)
	}
}
