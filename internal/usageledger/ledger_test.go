package usageledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
)

const instance = "0123456789abcdef0123456789abcdef"

func num(v int64) *int64 { return &v }
func subject(id string, up, down *int64) Counter {
	return Counter{BindingID: "main", UserID: id, EntitlementID: "period-" + id, Uplink: up, Downlink: down}
}
func openTest(t *testing.T) (*Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	l, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.BindIdentity("https://control.test\x00node-a"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}
func pending(t *testing.T, l *Ledger) []agentwire.UsageBatch {
	t.Helper()
	b, e := l.Pending(1000)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func ack(t *testing.T, l *Ledger, b agentwire.UsageBatch) {
	t.Helper()
	digest, e := agentwire.UsageDigest(b)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Ack(b.BatchID, digest); e != nil {
		t.Fatal(e)
	}
}

func TestDurableFirstSampleLostResponseReplayAndDigestAck(t *testing.T) {
	l, path := openTest(t)
	start := time.Now().Add(-time.Second)
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(25), num(15))}); e != nil {
		t.Fatal(e)
	}
	batches := pending(t, l)
	if len(batches) != 1 || batches[0].Sequence != 1 || batches[0].Items[0].UplinkBytes != 25 || batches[0].Items[0].DownlinkBytes != 15 || !batches[0].StartedAt.Equal(start) {
		t.Fatal("first counters were discarded", batches)
	}
	original, _ := json.Marshal(batches[0])
	l.Close()
	l, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	restored := pending(t, l)
	replay, _ := json.Marshal(restored[0])
	if string(original) != string(replay) {
		t.Fatal("restart mutated the pending batch")
	}
	if e = l.Ack(restored[0].BatchID, "forged-digest"); !errors.Is(e, ErrAck) {
		t.Fatal("forged ack accepted", e)
	}
	if len(pending(t, l)) != 1 {
		t.Fatal("forged acknowledgement lost batch")
	}
	ack(t, l, restored[0])
	ack(t, l, restored[0])
	if e = l.Sample(instance, start, start.Add(2*time.Second), []Counter{subject("a", num(40), num(35))}); e != nil {
		t.Fatal(e)
	}
	batches = pending(t, l)
	if len(batches) != 1 || batches[0].Sequence != 2 || batches[0].Items[0].UplinkBytes != 15 || batches[0].Items[0].DownlinkBytes != 20 {
		t.Fatal("cursor was not durable", batches)
	}
	var journal string
	var synchronous int
	if e = l.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); e != nil || journal != "wal" {
		t.Fatal("WAL disabled", journal, e)
	}
	if e = l.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); e != nil || synchronous != 2 {
		t.Fatal("FULL sync disabled", synchronous, e)
	}
}

func TestSparseCountersDoNotResetAndRegressionRollsBackWholeSample(t *testing.T) {
	l, _ := openTest(t)
	start := time.Now()
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(10), num(20)), subject("z", num(10), num(0))}); e != nil {
		t.Fatal(e)
	}
	ack(t, l, pending(t, l)[0])
	if e := l.Sample(instance, start, start.Add(2*time.Second), nil); e != nil {
		t.Fatal(e)
	}
	if len(pending(t, l)) != 0 {
		t.Fatal("empty observation generated a batch")
	}
	if e := l.Sample(instance, start, start.Add(3*time.Second), []Counter{subject("a", nil, num(25))}); e != nil {
		t.Fatal(e)
	}
	b := pending(t, l)[0]
	if len(b.Items) != 1 || b.Items[0].UplinkBytes != 0 || b.Items[0].DownlinkBytes != 5 {
		t.Fatal("missing direction reset the old cursor")
	}
	ack(t, l, b)
	// 'a' is updated before the regression on sorted subject 'z'. Neither update
	// nor last-sample time may commit when any counter goes backwards.
	if e := l.Sample(instance, start, start.Add(4*time.Second), []Counter{subject("a", num(20), nil), subject("z", num(9), num(0))}); !errors.Is(e, ErrRegression) {
		t.Fatal("reset within core instance accepted", e)
	}
	if len(pending(t, l)) != 0 {
		t.Fatal("failed sample generated a batch")
	}
	if e := l.Sample(instance, start, start.Add(4*time.Second), []Counter{subject("a", num(20), nil), subject("z", num(15), num(0))}); e != nil {
		t.Fatal(e)
	}
	b = pending(t, l)[0]
	if len(b.Items) != 2 || b.Sequence != 3 || b.Items[0].UplinkBytes != 10 || b.Items[1].UplinkBytes != 5 || !b.StartedAt.Equal(start.Add(3*time.Second)) {
		t.Fatal("failed sample advanced some cursors", b)
	}
	// New process IDs may legitimately restart at a lower cumulative count.
	if e := l.Sample("fedcba9876543210fedcba9876543210", start.Add(4*time.Second), start.Add(5*time.Second), []Counter{subject("a", num(2), num(3))}); e != nil {
		t.Fatal("new instance could not count from zero", e)
	}
}

func TestFullOutboxNeverAdvancesTrafficCursor(t *testing.T) {
	l, _ := openTest(t)
	l.maxBatches = 1
	start := time.Now()
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(10), num(0))}); e != nil {
		t.Fatal(e)
	}
	first := pending(t, l)[0]
	if e := l.Sample(instance, start, start.Add(2*time.Second), []Counter{subject("a", num(25), num(0))}); !errors.Is(e, ErrFull) {
		t.Fatal("full outbox did not block sample", e)
	}
	ack(t, l, first)
	if e := l.Sample(instance, start, start.Add(2*time.Second), []Counter{subject("a", num(25), num(0))}); e != nil {
		t.Fatal(e)
	}
	b := pending(t, l)[0]
	if b.Sequence != 2 || b.Items[0].UplinkBytes != 15 || !b.StartedAt.Equal(start.Add(time.Second)) {
		t.Fatal("full outbox lost traffic", b)
	}
	ack(t, l, b)
	l.maxBytes = 1
	if e := l.Sample(instance, start, start.Add(3*time.Second), []Counter{subject("a", num(30), num(0))}); !errors.Is(e, ErrFull) {
		t.Fatal("byte limit ignored", e)
	}
	l.maxBytes = MaxPendingBytes
	if e := l.Sample(instance, start, start.Add(3*time.Second), []Counter{subject("a", num(30), num(0))}); e != nil {
		t.Fatal(e)
	}
	if pending(t, l)[0].Items[0].UplinkBytes != 5 {
		t.Fatal("byte limit advanced cursor")
	}
}

func TestChunkedSampleCommitsAllBatchesOrNone(t *testing.T) {
	l, _ := openTest(t)
	start := time.Now()
	counters := []Counter{}
	for i := 0; i < 101; i++ {
		counters = append(counters, subject(fmt.Sprintf("u-%03d", i), num(1), num(0)))
	}
	l.maxBatches = 1
	if e := l.Sample(instance, start, start.Add(time.Second), counters); !errors.Is(e, ErrFull) {
		t.Fatal("partial chunk capacity accepted", e)
	}
	if len(pending(t, l)) != 0 {
		t.Fatal("partial chunk committed")
	}
	l.maxBatches = 2
	if e := l.Sample(instance, start, start.Add(time.Second), counters); e != nil {
		t.Fatal(e)
	}
	b := pending(t, l)
	if len(b) != 2 || len(b[0].Items) != 100 || len(b[1].Items) != 1 || b[0].Sequence != 1 || b[1].Sequence != 2 {
		t.Fatal("invalid chunking")
	}
	for _, batch := range b {
		data, _ := json.Marshal(batch)
		if len(data) >= 65536 {
			t.Fatal("batch too large for HTTP API")
		}
	}
}

func TestCounterAndSequenceOverflowDoNotCommit(t *testing.T) {
	l, _ := openTest(t)
	start := time.Now()
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(math.MaxInt64), num(1))}); !errors.Is(e, ErrOverflow) {
		t.Fatal("byte overflow accepted", e)
	}
	var count int
	if e := l.db.QueryRow(`SELECT count(*) FROM instances`).Scan(&count); e != nil || count != 0 {
		t.Fatal("failed first sample persisted instance")
	}
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(1), num(0))}); e != nil {
		t.Fatal(e)
	}
	ack(t, l, pending(t, l)[0])
	if _, e := l.db.Exec(`UPDATE instances SET sequence=? WHERE id=?`, int64(math.MaxInt64), instance); e != nil {
		t.Fatal(e)
	}
	if e := l.Sample(instance, start, start.Add(2*time.Second), []Counter{subject("a", num(2), num(0))}); !errors.Is(e, ErrOverflow) {
		t.Fatal("sequence overflow accepted", e)
	}
	if len(pending(t, l)) != 0 {
		t.Fatal("sequence overflow generated a batch")
	}
}

func TestPrivateFilesExclusiveOwnerAndIntervalValidation(t *testing.T) {
	l, path := openTest(t)
	if other, e := Open(path); e == nil {
		other.Close()
		t.Fatal("two writers own one ledger")
	}
	for _, name := range []string{path, path + "-wal", path + "-shm", path + ".lock"} {
		info, e := os.Stat(name)
		if e != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("non-private SQLite file", name, e)
		}
	}
	start := time.Now()
	if e := l.Sample(instance, start, start.Add(3*time.Minute), nil); e == nil {
		t.Fatal("oversized first interval accepted")
	}
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(5), nil)}); e != nil {
		t.Fatal(e)
	}
	if e := l.Sample(instance, start.Add(time.Second), start.Add(2*time.Second), nil); e == nil {
		t.Fatal("process start time changed")
	}
	if e := l.Sample(instance, start, start.Add(time.Second), nil); e == nil {
		t.Fatal("nonmonotonic sample accepted")
	}
	l.Close()
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if other, e := Open(path); e == nil {
		other.Close()
		t.Fatal("public database accepted")
	}
	publicParent := filepath.Join(t.TempDir(), "public")
	if e := os.Mkdir(publicParent, 0755); e != nil {
		t.Fatal(e)
	}
	if err := os.Chmod(publicParent, 0755); err != nil {
		t.Fatal(err)
	}
	if other, e := Open(filepath.Join(publicParent, "usage.sqlite")); e == nil {
		other.Close()
		t.Fatal("public parent accepted")
	}
}

func TestDatabaseWriteFailureRollsBackCursorAndOutboxTogether(t *testing.T) {
	l, _ := openTest(t)
	start := time.Now()
	if _, err := l.db.Exec(`CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'simulated durable write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(10), num(20))}); err == nil {
		t.Fatal("database write failure ignored")
	}
	if len(pending(t, l)) != 0 {
		t.Fatal("failed transaction left an outbox row")
	}
	if _, err := l.db.Exec(`DROP TRIGGER fail_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(10), num(20))}); err != nil {
		t.Fatal(err)
	}
	batch := pending(t, l)[0]
	if batch.Sequence != 1 || batch.Items[0].UplinkBytes != 10 || batch.Items[0].DownlinkBytes != 20 {
		t.Fatal("write failure advanced a cursor", batch)
	}
}

func TestIdentityBindingSurvivesRestartAndRejectsReassignment(t *testing.T) {
	l, path := openTest(t)
	start := time.Now()
	if e := l.Sample(instance, start, start.Add(time.Second), []Counter{subject("a", num(10), num(0))}); e != nil {
		t.Fatal(e)
	}
	original := pending(t, l)[0]
	l.Close()
	l, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	if e = l.BindIdentity("https://control.test\x00node-a"); e != nil {
		t.Fatal("same node restart failed", e)
	}
	for _, scope := range []string{"https://control.test\x00node-b", "https://other-control.test\x00node-a"} {
		if e = l.BindIdentity(scope); !errors.Is(e, ErrIdentity) {
			t.Fatal("cross-node/controller scope accepted", e)
		}
	}
	restored := pending(t, l)
	if len(restored) != 1 || restored[0].BatchID != original.BatchID {
		t.Fatal("reassignment failure discarded pending usage")
	}
	// Simulate a pre-binding database. Its historical ownership cannot be inferred
	// from a current caller, even if that caller holds a valid node credential.
	if _, e = l.db.Exec(`DELETE FROM metadata WHERE key='identity_scope'`); e != nil {
		t.Fatal(e)
	}
	l.Close()
	l, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	if e = l.BindIdentity("https://control.test\x00node-a"); !errors.Is(e, ErrIdentity) {
		t.Fatal("legacy pending usage assigned without proof", e)
	}
	if e = l.Sample(instance, start, start.Add(2*time.Second), []Counter{subject("a", num(20), num(0))}); !errors.Is(e, ErrIdentity) {
		t.Fatal("unbound sampling accepted", e)
	}
	if len(pending(t, l)) != 1 {
		t.Fatal("legacy pending traffic lost")
	}
}
