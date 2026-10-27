package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/store"
)

type testAccessGroup struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Revision int64    `json:"revision"`
	NodeIDs  []string `json:"node_ids"`
}

func accessJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func createAccessGroup(t *testing.T, f fixture, admin *http.Cookie, nodeIDs []string) testAccessGroup {
	t.Helper()
	status, data, _ := request(t, f, "POST", "/api/v1/admin/node-groups", accessJSON(t, map[string]any{"name": "套餐测试组", "node_ids": nodeIDs, "enabled": true}), admin)
	if status != 201 {
		t.Fatalf("create group %d %s", status, data)
	}
	var g testAccessGroup
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}
func TestAccessAdminBoundaryGroupRevisionAndAtomicMembership(t *testing.T) {
	f := setup(t, true)
	nodeID, _, admin := adminNode(t, f)
	_, user := register(t, f)
	for _, path := range []string{"/api/v1/admin/access", "/api/v1/admin/events"} {
		if status, _, _ := request(t, f, "GET", path, "", user); status != 403 {
			t.Fatalf("ordinary user read %s: %d", path, status)
		}
	}
	for _, path := range []string{"/api/v1/admin/node-groups", "/api/v1/admin/plans/explore/node-groups", "/api/v1/admin/events/any/retry", "/api/v1/admin/entitlements/any/revoke"} {
		if status, _, _ := request(t, f, "POST", path, "{}", user); status != 403 {
			t.Fatalf("ordinary user wrote %s: %d", path, status)
		}
	}
	group := createAccessGroup(t, f, admin, []string{nodeID})
	path := "/api/v1/admin/node-groups/" + group.ID
	body := map[string]any{"name": "changed", "node_ids": []string{nodeID}, "enabled": false, "revision": 0}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, body), admin); status != 400 {
		t.Fatal("missing revision accepted")
	}
	body["revision"] = group.Revision
	body["node_ids"] = []string{store.ID()}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, body), admin); status != 400 {
		t.Fatal("missing member accepted")
	}
	var name string
	var enabled bool
	var revision int64
	var members int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT name,enabled,revision,(SELECT count(*) FROM node_group_members WHERE group_id=$1) FROM node_groups WHERE id=$1`, group.ID).Scan(&name, &enabled, &revision, &members); err != nil {
		t.Fatal(err)
	}
	if name != group.Name || !enabled || revision != 1 || members != 1 {
		t.Fatal("invalid replacement partially applied")
	}
	body["node_ids"] = []string{nodeID, nodeID}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, body), admin); status != 400 {
		t.Fatal("duplicate group members accepted")
	}

	status, data, _ := request(t, f, "POST", "/api/v1/admin/nodes", `{"name":"second"}`, admin)
	if status != 201 {
		t.Fatal(status)
	}
	var second struct {
		ID string `json:"node_id"`
	}
	if err := json.Unmarshal(data, &second); err != nil {
		t.Fatal(err)
	}
	body["node_ids"] = []string{second.ID}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, body), admin); status != 200 {
		t.Fatal("valid group update failed", status)
	}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, body), admin); status != 409 {
		t.Fatal("stale group write accepted", status)
	}
	var payload []byte
	if err := f.db.DB.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE kind='node_group.changed' AND aggregate_id=$1`, group.ID+":2").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		NodeIDs []string `json:"node_ids"`
	}
	json.Unmarshal(payload, &event)
	if len(event.NodeIDs) != 2 || !strings.Contains(string(payload), nodeID) || !strings.Contains(string(payload), second.ID) {
		t.Fatal("removed and added nodes not both notified")
	}
	status, data, _ = request(t, f, "GET", "/api/v1/admin/access", "", admin)
	if status != 200 || !strings.Contains(string(data), `"revision":2`) || !strings.Contains(string(data), `"enabled":false`) {
		t.Fatal("access overview incomplete")
	}
}
func TestAccessPlanChangesOnlyFutureOrdersAndRequireRevision(t *testing.T) {
	f := setup(t, true)
	nodeID, _, admin := adminNode(t, f)
	user, _ := register(t, f)
	group := createAccessGroup(t, f, admin, []string{nodeID})
	path := "/api/v1/admin/plans/explore/node-groups"
	if status, data, _ := request(t, f, "POST", path, accessJSON(t, map[string]any{"revision": 1, "group_ids": []string{group.ID}}), admin); status != 200 {
		t.Fatalf("plan access %d %s", status, data)
	}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, map[string]any{"revision": 2, "group_ids": []string{group.ID, group.ID}}), admin); status != 400 {
		t.Fatal("duplicate plan group grants accepted")
	}
	first, err := f.db.CreateOrder(context.Background(), user.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, map[string]any{"revision": 2, "group_ids": []string{}}), admin); status != 200 {
		t.Fatal(status)
	}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, map[string]any{"revision": 2, "group_ids": []string{group.ID}}), admin); status != 409 {
		t.Fatal("stale plan mapping accepted")
	}
	if status, _, _ := request(t, f, "POST", path, accessJSON(t, map[string]any{"revision": 3, "group_ids": []string{store.ID()}}), admin); status != 400 {
		t.Fatal("invalid group mapping accepted")
	}
	if err = f.db.Settle(context.Background(), "access-test", "first", first.ID, user.ID, first.Price, first.Currency); err != nil {
		t.Fatal(err)
	}
	second, err := f.db.CreateOrder(context.Background(), user.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Settle(context.Background(), "access-test", "second", second.ID, user.ID, second.Price, second.Currency); err != nil {
		t.Fatal(err)
	}
	var oldGroups, newGroups int
	if err = f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM entitlement_node_groups g JOIN entitlement_periods e ON e.id=g.entitlement_period_id WHERE e.order_id=$1`, first.ID).Scan(&oldGroups); err != nil {
		t.Fatal(err)
	}
	if err = f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM entitlement_node_groups g JOIN entitlement_periods e ON e.id=g.entitlement_period_id WHERE e.order_id=$1`, second.ID).Scan(&newGroups); err != nil {
		t.Fatal(err)
	}
	if oldGroups != 1 || newGroups != 0 {
		t.Fatal("plan edit changed existing order authorization snapshot", oldGroups, newGroups)
	}
	var revision int64
	f.db.DB.QueryRow(context.Background(), `SELECT access_revision FROM plans WHERE id='explore'`).Scan(&revision)
	if revision != 3 {
		t.Fatal("failed edit advanced plan revision")
	}
}
func TestAccessEntitlementRevokeIsAtomicAndIdempotent(t *testing.T) {
	f := setup(t, true)
	_, _, admin := adminNode(t, f)
	user, _ := register(t, f)
	order, err := f.db.CreateOrder(context.Background(), user.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Settle(context.Background(), "access-test", "paid", order.ID, user.ID, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	var period string
	if err = f.db.DB.QueryRow(context.Background(), `SELECT id FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&period); err != nil {
		t.Fatal(err)
	}
	// Event persistence failure must roll back the authority change itself.
	if _, err = f.db.DB.Exec(context.Background(), `CREATE FUNCTION reject_revoke_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='entitlement.revoked' THEN RAISE EXCEPTION 'test rejection'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_revoke_event BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_revoke_event()`); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/entitlements/" + period + "/revoke"
	if status, _, _ := request(t, f, "POST", path, "{}", admin); status != 500 {
		t.Fatal("event rejection did not reject revoke")
	}
	var revoked bool
	if err = f.db.DB.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM entitlement_periods WHERE id=$1`, period).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("revoke persisted without its event")
	}
	if _, err = f.db.DB.Exec(context.Background(), `DROP TRIGGER reject_revoke_event ON outbox_events`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if status, _, _ := request(t, f, "POST", path, "{}", admin); status != 200 {
			t.Fatal("revoke failed", status)
		}
	}
	var events, audits int
	f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE kind='entitlement.revoked' AND aggregate_id=$1`, period).Scan(&events)
	f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='entitlement.revoked' AND resource_id=$1`, period).Scan(&audits)
	if events != 1 || audits != 1 {
		t.Fatal("duplicate revoke repeated event/audit", events, audits)
	}
}
func TestAccessFailedEventRetryAndReadOnlyProcessedEvents(t *testing.T) {
	f := setup(t, true)
	_, _, admin := adminNode(t, f)
	id := store.ID()
	secret := store.ID() + store.ID()
	if _, err := f.db.DB.Exec(context.Background(), `INSERT INTO outbox_events(id,kind,aggregate_id,payload,attempts,last_error,dead_lettered_at) VALUES($1,'poison',$1,jsonb_build_object('private',$2::text),5,'unsupported_event',now())`, id, secret); err != nil {
		t.Fatal(err)
	}
	status, data, _ := request(t, f, "GET", "/api/v1/admin/events", "", admin)
	if status != 200 || strings.Contains(string(data), secret) || !strings.Contains(string(data), `"attempt_count":5`) {
		t.Fatal("event list leaked payload or missing retry state")
	}
	if status, _, _ := request(t, f, "POST", "/api/v1/admin/events/"+id+"/retry", "{}", admin); status != 200 {
		t.Fatal("dead event retry failed")
	}
	var attempts int
	var cleared bool
	if err := f.db.DB.QueryRow(context.Background(), `SELECT attempts,dead_lettered_at IS NULL AND last_error='' AND available_at<=now() FROM outbox_events WHERE id=$1`, id).Scan(&attempts, &cleared); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || !cleared {
		t.Fatal("retry did not clear failure state")
	}
	if status, _, _ := request(t, f, "POST", "/api/v1/admin/events/"+id+"/retry", "{}", admin); status != 409 {
		t.Fatal("fresh pending event can be retried")
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE outbox_events SET processed_at=now(),attempts=1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := request(t, f, "POST", "/api/v1/admin/events/"+id+"/retry", "{}", admin); status != 409 {
		t.Fatal("completed event replay allowed")
	}
}
