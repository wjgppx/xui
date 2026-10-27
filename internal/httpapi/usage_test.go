package httpapi_test

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
)

type usageFixture struct {
	f                               fixture
	nodeID, token, userID, periodID string
	batch                           agentwire.UsageBatch
}

func prepareUsage(t *testing.T) usageFixture {
	t.Helper()
	f := setup(t, true)
	nodeID, enrollment, _ := adminNode(t, f)
	user, _ := register(t, f)
	token := store.ID() + store.ID()
	if code, b := machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: token}); code != 200 {
		t.Fatalf("enroll %d %s", code, b)
	}
	order, err := f.db.CreateOrder(context.Background(), user.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Settle(context.Background(), "usage-test", "initial", order.ID, user.ID, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	var period string
	if err = f.db.DB.QueryRow(context.Background(), `SELECT id FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&period); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = f.db.DB.Exec(context.Background(), `INSERT INTO node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_from,authorized_until) VALUES($1,'main',$2,$3,$4,$5)`, nodeID, user.ID, period, now.Add(-time.Second), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return usageFixture{f, nodeID, token, user.ID, period, agentwire.UsageBatch{BatchID: store.ID(), CoreInstanceID: store.ID(), Sequence: 1, StartedAt: now, EndedAt: now.Add(time.Millisecond), Items: []agentwire.UsageItem{{BindingID: "main", UserID: user.ID, EntitlementID: period, UplinkBytes: 17, DownlinkBytes: 23}}}}
}
func usageTotals(t *testing.T, u usageFixture) (used int64, batches, items int) {
	t.Helper()
	ctx := context.Background()
	if err := u.f.db.DB.QueryRow(ctx, `SELECT used_bytes FROM entitlement_periods WHERE id=$1`, u.periodID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := u.f.db.DB.QueryRow(ctx, `SELECT count(*) FROM usage_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := u.f.db.DB.QueryRow(ctx, `SELECT count(*) FROM usage_items`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	return
}

func TestUsageConcurrentRetryAndConflictingIdentity(t *testing.T) {
	u := prepareUsage(t)
	digest, err := agentwire.UsageDigest(u.batch)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, b := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, u.batch)
			if code != 200 {
				t.Errorf("retry %d %s", code, b)
				return
			}
			var ack agentwire.UsageAck
			if err := json.Unmarshal(b, &ack); err != nil || ack.BatchID != u.batch.BatchID || ack.Digest != digest {
				t.Errorf("incorrect acknowledgement %s", b)
			}
		}()
	}
	wg.Wait()
	if used, batches, items := usageTotals(t, u); used != 40 || batches != 1 || items != 1 {
		t.Fatalf("duplicate charged: %d/%d/%d", used, batches, items)
	}
	changed := u.batch
	changed.Items = append([]agentwire.UsageItem(nil), u.batch.Items...)
	changed.Items[0].UplinkBytes++
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, changed); code != 409 {
		t.Fatal("batch ID conflict accepted", code)
	}
	changed = u.batch
	changed.BatchID = store.ID()
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, changed); code != 409 {
		t.Fatal("alternate ID reused sample sequence", code)
	}
	if used, batches, _ := usageTotals(t, u); used != 40 || batches != 1 {
		t.Fatal("conflict changed quota")
	}
}

func TestUsageOwnershipAndAuthorizationFailureRollBackEntireBatch(t *testing.T) {
	u := prepareUsage(t)
	other, _ := register(t, u.f)
	scenarios := []agentwire.UsageItem{
		{BindingID: "unissued", UserID: u.userID, EntitlementID: u.periodID, UplinkBytes: 1},
		{BindingID: "main", UserID: other.ID, EntitlementID: u.periodID, UplinkBytes: 1},
		{BindingID: "main", UserID: u.userID, EntitlementID: store.ID(), UplinkBytes: 1},
	}
	for _, item := range scenarios {
		bad := u.batch
		bad.Items = append(append([]agentwire.UsageItem(nil), u.batch.Items...), item)
		if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, bad); code != 403 {
			t.Fatal("unauthorized usage accepted", code)
		}
	}
	otherNode, otherEnrollment, _ := adminNode(t, u.f)
	otherToken := store.ID() + store.ID()
	if code, _ := machine(t, u.f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: otherEnrollment, AgentToken: otherToken}); code != 200 {
		t.Fatal(code)
	}
	if otherNode == u.nodeID {
		t.Fatal("fixture node collision")
	}
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", otherToken, u.batch); code != 403 {
		t.Fatal("other node reused issued grant", code)
	}
	if used, batches, items := usageTotals(t, u); used != 0 || batches != 0 || items != 0 {
		t.Fatal("invalid multi-item batch partially committed", used, batches, items)
	}
	// Cookies do not replace machine authentication, and revocation is enforced.
	_, cookie := register(t, u.f)
	if code, _, _ := request(t, u.f, "POST", "/agent/v1/usage-batches", `{}`, cookie); code != 401 {
		t.Fatal("web session accepted for usage", code)
	}
	if _, err := u.f.db.DB.Exec(context.Background(), `UPDATE nodes SET revoked_at=now() WHERE id=$1`, u.nodeID); err != nil {
		t.Fatal(err)
	}
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, u.batch); code != 401 {
		t.Fatal("revoked node billed usage", code)
	}
}

func TestUsageDelayedExpiredPeriodAndExactOverageAccounting(t *testing.T) {
	u := prepareUsage(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	if _, err := u.f.db.DB.Exec(ctx, `UPDATE entitlement_periods SET starts_at=$1,ends_at=$2,used_bytes=quota_bytes-1 WHERE id=$3`, base.Add(-time.Minute), base.Add(time.Minute), u.periodID); err != nil {
		t.Fatal(err)
	}
	if _, err := u.f.db.DB.Exec(ctx, `INSERT INTO node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_from,authorized_until) VALUES($1,'main',$2,$3,$4,$5)`, u.nodeID, u.userID, u.periodID, base, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	u.batch.StartedAt = base.Add(time.Second)
	u.batch.EndedAt = base.Add(2 * time.Second)
	if code, b := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, u.batch); code != 200 {
		t.Fatalf("historical valid usage rejected %d %s", code, b)
	}
	var used, quota int64
	if err := u.f.db.DB.QueryRow(ctx, `SELECT used_bytes,quota_bytes FROM entitlement_periods WHERE id=$1`, u.periodID).Scan(&used, &quota); err != nil {
		t.Fatal(err)
	}
	if used != quota+39 {
		t.Fatal("overage clamped or attributed incorrectly", used, quota)
	}
	// An equivalent timestamp offset must produce the same content digest.
	u.batch.StartedAt = u.batch.StartedAt.In(time.FixedZone("offset", 8*60*60))
	u.batch.EndedAt = u.batch.EndedAt.In(time.FixedZone("offset", 8*60*60))
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, u.batch); code != 200 {
		t.Fatal("equivalent UTC retry conflicted", code)
	}
}

func TestUsageOverflowAndInvalidIntervalsDoNotMutateBalances(t *testing.T) {
	u := prepareUsage(t)
	bad := u.batch
	bad.Items = append([]agentwire.UsageItem(nil), u.batch.Items...)
	bad.Items[0].UplinkBytes = math.MaxInt64
	bad.Items[0].DownlinkBytes = 1
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, bad); code != 400 {
		t.Fatal("overflowing direction sum accepted", code)
	}
	bad = u.batch
	bad.Items = []agentwire.UsageItem{u.batch.Items[0], u.batch.Items[0]}
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, bad); code != 400 {
		t.Fatal("duplicate stream accepted", code)
	}
	for _, end := range []time.Time{u.batch.StartedAt, u.batch.StartedAt.Add(3 * time.Minute), time.Now().Add(time.Minute)} {
		bad = u.batch
		bad.EndedAt = end
		if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, bad); code != 400 {
			t.Fatal("invalid sample interval accepted", code)
		}
	}
	if _, err := u.f.db.DB.Exec(context.Background(), `UPDATE entitlement_periods SET used_bytes=$1 WHERE id=$2`, int64(math.MaxInt64-10), u.periodID); err != nil {
		t.Fatal(err)
	}
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, u.batch); code != 409 {
		t.Fatal("balance overflow accepted", code)
	}
	if used, batches, items := usageTotals(t, u); used != math.MaxInt64-10 || batches != 0 || items != 0 {
		t.Fatal("overflow partially committed", used, batches, items)
	}
}

func TestUsageAuthorizationGapsArePreserved(t *testing.T) {
	u := prepareUsage(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-10 * time.Minute)
	for _, offset := range []time.Duration{0, 4 * time.Minute} {
		if _, err := u.f.db.DB.Exec(ctx, `INSERT INTO node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_from,authorized_until) VALUES($1,'main',$2,$3,$4,$5)`, u.nodeID, u.userID, u.periodID, base.Add(offset), base.Add(offset+time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	u.batch.StartedAt = base.Add(2 * time.Minute)
	u.batch.EndedAt = base.Add(2*time.Minute + time.Second)
	if code, _ := machine(t, u.f, "POST", "/agent/v1/usage-batches", u.token, u.batch); code != 403 {
		t.Fatal("unissued gap charged", code)
	}
}

func TestUsageConcurrentNodesDebitSameEntitlementExactly(t *testing.T) {
	u := prepareUsage(t)
	secondID, enrollment, _ := adminNode(t, u.f)
	secondToken := store.ID() + store.ID()
	if code, _ := machine(t, u.f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: secondToken}); code != 200 {
		t.Fatal(code)
	}
	if _, err := u.f.db.DB.Exec(context.Background(), `INSERT INTO node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_from,authorized_until) VALUES($1,'main',$2,$3,$4,$5)`, secondID, u.userID, u.periodID, u.batch.StartedAt.Add(-time.Second), u.batch.EndedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			batch := u.batch
			batch.BatchID = store.ID()
			batch.Sequence = int64(i/2 + 1)
			token := u.token
			if i%2 == 1 {
				token = secondToken
			}
			if code, b := machine(t, u.f, "POST", "/agent/v1/usage-batches", token, batch); code != 200 {
				t.Errorf("concurrent debit %d %s", code, b)
			}
		}(i)
	}
	wg.Wait()
	if used, batches, items := usageTotals(t, u); used != 12*40 || batches != 12 || items != 12 {
		t.Fatal("concurrent debit lost data", used, batches, items)
	}
}
