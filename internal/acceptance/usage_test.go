//go:build acceptance

// Package acceptance_test runs explicit, bounded acceptance workloads against
// isolated databases. It is excluded from the ordinary unit-test invocation.
package acceptance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"github.com/wjgppx/xui/internal/usageledger"
)

var durationPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?(?:ms|s|m|h)$`)

type workloadConfig struct {
	duration, interval time.Duration
	workers            int
	reportPath         string
}

func durationValue(value string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	if !durationPattern.MatchString(value) {
		return 0, errors.New("acceptance duration requires one numeric ms/s/m/h unit")
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < minimum || duration > maximum {
		return 0, errors.New("acceptance duration is outside allowed bounds")
	}
	return duration, nil
}

func config(duration, interval, workers, path string) (workloadConfig, error) {
	c := workloadConfig{reportPath: path, workers: 4}
	var err error
	if c.duration, err = durationValue(duration, 10*time.Second, time.Second, 24*time.Hour); err != nil {
		return c, err
	}
	if c.interval, err = durationValue(interval, 200*time.Millisecond, 100*time.Millisecond, 10*time.Second); err != nil {
		return c, err
	}
	if c.interval > c.duration/4 {
		return c, errors.New("acceptance duration must allow at least four samples per worker")
	}
	if workers != "" {
		c.workers, err = strconv.Atoi(workers)
		if err != nil || c.workers < 1 || c.workers > 32 {
			return c, errors.New("acceptance workers must be 1 through 32")
		}
	}
	return c, nil
}

type latencyHistogram struct {
	Counts [16]int64 `json:"counts"`
	Total  int64     `json:"count"`
	MaxMS  float64   `json:"max_ms"`
}

var latencyBoundsMS = [15]float64{0.1, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

func (h *latencyHistogram) add(value time.Duration) {
	ms := float64(value) / float64(time.Millisecond)
	bucket := len(latencyBoundsMS)
	for i, bound := range latencyBoundsMS {
		if ms <= bound {
			bucket = i
			break
		}
	}
	h.Counts[bucket]++
	h.Total++
	if ms > h.MaxMS {
		h.MaxMS = ms
	}
}

func (h *latencyHistogram) merge(other latencyHistogram) {
	h.Total += other.Total
	for i, count := range other.Counts {
		h.Counts[i] += count
	}
	if other.MaxMS > h.MaxMS {
		h.MaxMS = other.MaxMS
	}
}

func (h latencyHistogram) percentile(q float64) float64 {
	if h.Total == 0 {
		return 0
	}
	target, count := int64(math.Ceil(float64(h.Total)*q)), int64(0)
	for i, value := range h.Counts {
		count += value
		if count >= target {
			if i == len(latencyBoundsMS) {
				// Overflow uses the actual observed maximum, never a lower
				// bound mislabeled as a percentile upper bound.
				return h.MaxMS
			}
			return latencyBoundsMS[i]
		}
	}
	return h.MaxMS
}

type transport struct {
	base                                       http.RoundTripper
	offline, lost                              bool
	offlineAttempts, wireAttempts, lostReplies int64
}

func (tr *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if tr.offline {
		tr.offlineAttempts++
		return nil, errors.New("injected controller transport outage")
	}
	tr.wireAttempts++
	response, err := tr.base.RoundTrip(r)
	if err == nil && !tr.lost && r.URL.Path == "/agent/v1/usage-batches" && response.StatusCode == http.StatusOK {
		// The real HTTP handler already committed. Drop only its first reply;
		// the client must replay exactly the durable pending batch.
		tr.lost, tr.lostReplies = true, tr.lostReplies+1
		response.Body.Close()
		response.StatusCode = http.StatusServiceUnavailable
		response.Body = io.NopCloser(strings.NewReader(`{"error":"injected lost acknowledgement"}`))
	}
	return response, err
}

type workerResult struct {
	measured, sampled, samples int64
	remaining, restarts        int64
	outages, wire, lost        int64
	latency                    latencyHistogram
	failure                    string
}

type acceptanceReport struct {
	Version                     string    `json:"version"`
	Status                      string    `json:"status"`
	StartedAt                   time.Time `json:"started_at"`
	CompletedAt                 time.Time `json:"completed_at"`
	RequestedDurationSeconds    float64   `json:"requested_duration_seconds"`
	ElapsedSeconds              float64   `json:"elapsed_seconds"`
	Workers                     int       `json:"workers"`
	SampleIntervalMS            float64   `json:"sample_interval_ms"`
	Samples                     int64     `json:"samples"`
	MeasuredPayloadBytes        int64     `json:"measured_payload_bytes"`
	PersistedSampleBytes        int64     `json:"persisted_sample_bytes"`
	AcceptedPayloadBytes        int64     `json:"accepted_payload_bytes"`
	UsageItemBytes              int64     `json:"usage_item_bytes"`
	EntitlementUsedBytes        int64     `json:"entitlement_used_bytes"`
	ObservedUnpersistedBytes    int64     `json:"observed_unpersisted_payload_bytes"`
	AccountingDifferenceBytes   int64     `json:"accounting_difference_bytes"`
	UniqueBatches               int64     `json:"unique_batches"`
	WireAttempts                int64     `json:"wire_attempts"`
	ReplayRequests              int64     `json:"replay_requests"`
	InjectedOutageAttempts      int64     `json:"injected_outage_attempts"`
	InjectedLostReplies         int64     `json:"injected_lost_replies"`
	LedgerReopens               int64     `json:"ledger_reopens"`
	PendingBatches              int64     `json:"pending_batches"`
	FailureCodes                []string  `json:"failure_codes"`
	FlushLatencyP50UpperMS      float64   `json:"flush_latency_p50_upper_ms"`
	FlushLatencyP95UpperMS      float64   `json:"flush_latency_p95_upper_ms"`
	FlushLatencySamples         int64     `json:"flush_latency_samples"`
	FlushLatencyMaxMS           float64   `json:"flush_latency_max_ms"`
	FlushLatencyOver10sSamples  int64     `json:"flush_latency_over_10s_samples"`
	CountingBoundary            string    `json:"counting_boundary"`
	CoreOverheadMeasured        bool      `json:"core_overhead_measured"`
	UnobservedCrashLossMeasured bool      `json:"unobserved_crash_loss_measured"`
}

func writeReport(path string, report acceptanceReport) error {
	if path == "" {
		return nil
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return errors.New("acceptance report directory unavailable")
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("acceptance report directory must be private")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.New("acceptance report encoding failed")
	}
	file, err := os.CreateTemp(parent, ".usage-acceptance-*")
	if err != nil {
		return errors.New("acceptance report unavailable")
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("acceptance report could not be persisted")
	}
	// A hard link publishes atomically without overwriting an existing report
	// or following a destination symlink. Temporary files are mode0600.
	if err = os.Link(name, path); err != nil {
		return errors.New("acceptance report destination already exists or is unavailable")
	}
	dir, err := os.Open(parent)
	if err != nil {
		return errors.New("acceptance report directory unavailable")
	}
	defer dir.Close()
	return dir.Sync()
}

func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("local payload probe unavailable")
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(connection)
		}
	}()
	return listener.Addr().String()
}

func worker(c workloadConfig, origin, probe, node, token, user, period, path string, start, stop time.Time) (result workerResult) {
	ledger, err := usageledger.Open(path)
	if err != nil {
		result.failure = "ledger_open_failed"
		return result
	}
	defer func() {
		if ledger != nil {
			ledger.Close()
		}
	}()
	tr := &transport{base: http.DefaultTransport, offline: true}
	defer func() { result.outages, result.wire, result.lost = tr.offlineAttempts, tr.wireAttempts, tr.lostReplies }()
	client := &nodeagent.Client{State: nodeagent.State{Controller: origin, NodeID: node, AgentToken: token}, Outbox: ledger, HTTP: &http.Client{Transport: tr, Timeout: 6 * time.Second}}
	if err = ledger.BindIdentity(origin + "\x00" + node); err != nil {
		result.failure = "ledger_identity_failed"
		return result
	}
	connection, err := net.DialTimeout("tcp", probe, time.Second)
	if err != nil {
		result.failure = "payload_probe_unavailable"
		return result
	}
	defer connection.Close()
	payload := bytes.Repeat([]byte("acceptance-payload-"), 228)
	echoed := make([]byte, len(payload))
	instance, up, down := store.ID(), int64(0), int64(0)
	for time.Now().Before(stop) {
		iteration := time.Now()
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		written := 0
		for written < len(payload) {
			n, e := connection.Write(payload[written:])
			written += n
			if e != nil {
				result.measured += int64(written)
				result.failure = "payload_write_failed"
				return result
			}
		}
		n, e := io.ReadFull(connection, echoed)
		up, down = up+int64(written), down+int64(n)
		result.measured = up + down
		if e != nil || !bytes.Equal(payload, echoed) {
			result.failure = "payload_read_failed"
			return result
		}
		if err = ledger.Sample(instance, start, time.Now().UTC(), []usageledger.Counter{{BindingID: "main", UserID: user, EntitlementID: period, Uplink: &up, Downlink: &down}}); err != nil {
			result.failure = "sample_store_failed"
			return result
		}
		result.samples++
		result.sampled = up + down
		if result.samples == 2 {
			// Model an Agent restart while both samples are still unacknowledged.
			if err = ledger.Close(); err == nil {
				ledger, err = usageledger.Open(path)
			}
			if err != nil {
				result.failure = "ledger_reopen_failed"
				return result
			}
			client.Outbox = ledger
			result.restarts++
		}
		tr.offline = result.samples <= 2
		before := time.Now()
		err = client.FlushUsage(context.Background())
		result.latency.add(time.Since(before))
		if err != nil && result.samples > 3 {
			result.failure = "usage_delivery_failed"
			return result
		}
		if delay := c.interval - time.Since(iteration); delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}
	}
	tr.offline = false
	for i := 0; i < 4; i++ {
		before := time.Now()
		err = client.FlushUsage(context.Background())
		result.latency.add(time.Since(before))
		if err == nil {
			break
		}
	}
	if err != nil {
		result.failure = "final_delivery_failed"
	}
	result.remaining, _, err = ledger.Stats()
	if err != nil {
		result.failure = "ledger_stats_failed"
	}
	return result
}

func TestLoadUsageNetworkRecoveryAndExactAccounting(t *testing.T) {
	c, err := config(os.Getenv("XUI_ACCEPTANCE_DURATION"), os.Getenv("XUI_ACCEPTANCE_INTERVAL"), os.Getenv("XUI_ACCEPTANCE_WORKERS"), os.Getenv("XUI_ACCEPTANCE_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	s := testutil.Database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	user := testutil.User(t, s)
	order, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal("acceptance entitlement setup failed")
	}
	if err = s.Settle(ctx, "acceptance", store.ID(), order.ID, user, order.Price, order.Currency); err != nil {
		t.Fatal("acceptance entitlement setup failed")
	}
	var period string
	if err = s.DB.QueryRow(ctx, `SELECT id FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&period); err != nil {
		t.Fatal("acceptance entitlement unavailable")
	}
	server := httptest.NewServer(httpapi.New(s, httpapi.Config{Origin: "http://127.0.0.1", Development: true}).Handler())
	defer server.Close()
	probe := echoServer(t)
	nodes, tokens := make([]string, c.workers), make([]string, c.workers)
	for i := range nodes {
		nodes[i], tokens[i] = store.ID(), store.ID()+store.ID()
		digest := sha256.Sum256([]byte(tokens[i]))
		_, err = s.DB.Exec(ctx, `INSERT INTO nodes(id,name,enrollment_hash,enrollment_expires_at,agent_token_hash,enrolled_at) VALUES($1,'acceptance node',$2,clock_timestamp()+interval '1 day',$3,clock_timestamp())`, nodes[i], store.ID(), hex.EncodeToString(digest[:]))
		if err == nil {
			_, err = s.DB.Exec(ctx, `INSERT INTO node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_from,authorized_until) VALUES($1,'main',$2,$3,$4,$5)`, nodes[i], user, period, time.Now().Add(-time.Minute), time.Now().Add(c.duration+5*time.Minute))
		}
		if err != nil {
			t.Fatal("acceptance node setup failed")
		}
	}
	started := time.Now().UTC()
	stop := started.Add(c.duration)
	results := make([]workerResult, c.workers)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		path := filepath.Join(t.TempDir(), "usage.sqlite")
		go func(i int) {
			defer group.Done()
			results[i] = worker(c, server.URL, probe, nodes[i], tokens[i], user, period, path, started.Add(-time.Millisecond), stop)
		}(i)
	}
	group.Wait()
	report := acceptanceReport{Version: "usage-acceptance-v1", Status: "failed", StartedAt: started, CompletedAt: time.Now().UTC(), RequestedDurationSeconds: c.duration.Seconds(), Workers: c.workers, SampleIntervalMS: float64(c.interval) / float64(time.Millisecond), FailureCodes: []string{}, CountingBoundary: "actual loopback TCP application payload copied into sampled counters; not Xray/TLS framing or unobserved crash-window loss"}
	latency := latencyHistogram{}
	for _, result := range results {
		report.MeasuredPayloadBytes += result.measured
		report.PersistedSampleBytes += result.sampled
		report.Samples += result.samples
		report.PendingBatches += result.remaining
		report.InjectedOutageAttempts += result.outages
		report.WireAttempts += result.wire
		report.InjectedLostReplies += result.lost
		report.LedgerReopens += result.restarts
		if result.failure != "" {
			report.FailureCodes = append(report.FailureCodes, result.failure)
		}
		latency.merge(result.latency)
	}
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer queryCancel()
	err = s.DB.QueryRow(queryCtx, `SELECT (SELECT count(*) FROM usage_batches),(SELECT COALESCE(sum(uplink_bytes+downlink_bytes),0)::bigint FROM usage_items),(SELECT used_bytes FROM entitlement_periods WHERE id=$1),(SELECT COALESCE(sum((i->>'uplink_bytes')::numeric+(i->>'downlink_bytes')::numeric),0)::bigint FROM usage_batches b CROSS JOIN LATERAL jsonb_array_elements(b.payload->'items') i)`, period).Scan(&report.UniqueBatches, &report.UsageItemBytes, &report.EntitlementUsedBytes, &report.AcceptedPayloadBytes)
	if err != nil {
		report.FailureCodes = append(report.FailureCodes, "accounting_evidence_unavailable")
	}
	report.ElapsedSeconds = report.CompletedAt.Sub(started).Seconds()
	report.ObservedUnpersistedBytes = report.MeasuredPayloadBytes - report.PersistedSampleBytes
	report.AccountingDifferenceBytes = report.PersistedSampleBytes - report.EntitlementUsedBytes
	report.ReplayRequests = report.WireAttempts - report.UniqueBatches
	report.FlushLatencyP50UpperMS, report.FlushLatencyP95UpperMS, report.FlushLatencySamples = latency.percentile(0.5), latency.percentile(0.95), latency.Total
	report.FlushLatencyMaxMS, report.FlushLatencyOver10sSamples = latency.MaxMS, latency.Counts[len(latencyBoundsMS)]
	valid := len(report.FailureCodes) == 0 && report.Samples >= int64(c.workers*4) && report.UniqueBatches == report.Samples && report.PendingBatches == 0 && report.MeasuredPayloadBytes == report.PersistedSampleBytes && report.PersistedSampleBytes == report.AcceptedPayloadBytes && report.AcceptedPayloadBytes == report.UsageItemBytes && report.UsageItemBytes == report.EntitlementUsedBytes && report.InjectedOutageAttempts == int64(c.workers*2) && report.InjectedLostReplies == int64(c.workers) && report.ReplayRequests == int64(c.workers) && report.LedgerReopens == int64(c.workers)
	if valid {
		report.Status = "passed"
	}
	if err = writeReport(c.reportPath, report); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatalf("acceptance failed: failure_codes=%v samples=%d unique_batches=%d accounting_difference=%d pending=%d", report.FailureCodes, report.Samples, report.UniqueBatches, report.AccountingDifferenceBytes, report.PendingBatches)
	}
	t.Logf("acceptance passed: elapsed=%.3fs nodes=%d samples=%d payload_bytes=%d replay_requests=%d pending=%d p95_flush_upper_ms=%.1f", report.ElapsedSeconds, c.workers, report.Samples, report.MeasuredPayloadBytes, report.ReplayRequests, report.PendingBatches, report.FlushLatencyP95UpperMS)
}

func TestAcceptanceConfigurationRejectsUnboundedOrUnreproducibleWorkloads(t *testing.T) {
	for _, values := range [][3]string{{"0s", "", ""}, {"25h", "", ""}, {"10s", "1ms", ""}, {"10s", "", "33"}, {"10s", "", "0"}, {"1s", "1s", "1"}, {"untrusted", "", "1"}} {
		if _, err := config(values[0], values[1], values[2], ""); err == nil {
			t.Fatal("invalid acceptance workload accepted")
		}
	}
	if _, err := config("30m", "200ms", "4", ""); err != nil {
		t.Fatal("bounded soak configuration rejected", err)
	}
}

func TestAcceptanceReportsArePrivateAndCannotOverwriteEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(path, acceptanceReport{Version: "fixture", FailureCodes: []string{}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("acceptance evidence is not private", err)
	}
	if err = writeReport(path, acceptanceReport{}); err == nil {
		t.Fatal("acceptance evidence overwritten")
	}
	link := filepath.Join(filepath.Dir(path), "report-link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err = writeReport(link, acceptanceReport{}); err == nil {
		t.Fatal("acceptance evidence destination followed a symlink")
	}
}

func TestLatencyHistogramOverflowAndMergeRetainRealUpperBounds(t *testing.T) {
	var short, long latencyHistogram
	short.add(10 * time.Millisecond)
	short.add(10 * time.Second)
	long.add(12 * time.Second)
	long.add(25*time.Second + 500*time.Millisecond)
	short.merge(long)
	if short.Total != 4 || short.Counts[len(latencyBoundsMS)] != 2 || short.MaxMS != 25500 {
		t.Fatal("latency histogram lost overflow samples or actual maximum")
	}
	if short.percentile(.5) != 10000 || short.percentile(.95) != 25500 {
		t.Fatal("latency percentile upper bound truncates a slow flush")
	}
	var empty latencyHistogram
	if empty.percentile(.95) != 0 {
		t.Fatal("empty latency histogram reports an observation")
	}
	// A merged overflow must stay finite and serializable in acceptance evidence.
	report := acceptanceReport{FlushLatencyP95UpperMS: short.percentile(.95), FlushLatencyMaxMS: short.MaxMS, FlushLatencyOver10sSamples: short.Counts[len(latencyBoundsMS)]}
	if _, err := json.Marshal(report); err != nil {
		t.Fatal("latency evidence is not JSON serializable")
	}
}
