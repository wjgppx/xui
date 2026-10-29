package usageledger

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
)

type crashReady struct {
	Batch  agentwire.UsageBatch `json:"batch"`
	Digest string               `json:"digest"`
}

func TestSIGKILLRecoversCommittedWALAndDiscardsUncommittedSample(t *testing.T) {
	if mode := os.Getenv("XUI_LEDGER_CRASH_HELPER"); mode != "" {
		crashLedgerChild(t, mode, os.Getenv("XUI_LEDGER_CRASH_PATH"))
		return
	}
	for _, mode := range []string{"committed", "uncommitted"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage", "ledger.sqlite")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSIGKILLRecoversCommittedWALAndDiscardsUncommittedSample$", "-test.v=false")
			cmd.Env = append(os.Environ(), "XUI_LEDGER_CRASH_HELPER="+mode, "XUI_LEDGER_CRASH_PATH="+path)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			ready := make(chan []byte, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				if scanner.Scan() {
					ready <- append([]byte(nil), scanner.Bytes()...)
				} else {
					ready <- nil
				}
			}()
			var saved crashReady
			select {
			case data := <-ready:
				if len(data) == 0 || json.Unmarshal(data, &saved) != nil || saved.Batch.BatchID == "" || saved.Digest == "" {
					t.Fatal("crash helper did not reach its persistence boundary")
				}
			case <-ctx.Done():
				t.Fatal("crash helper timed out")
			}
			info, err := os.Stat(path + "-wal")
			if err != nil || info.Size() == 0 {
				t.Fatal("fixture did not exercise an open WAL database", err)
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("fixture was not terminated by SIGKILL")
			}
			ledger, err := Open(path)
			if err != nil {
				t.Fatal("WAL could not recover after abrupt death", err)
			}
			defer ledger.Close()
			if err = ledger.BindIdentity("https://crash.test\x00crash-node"); err != nil {
				t.Fatal("identity binding did not survive", err)
			}
			batches, err := ledger.Pending(10)
			if err != nil || len(batches) != 1 || batches[0].BatchID != saved.Batch.BatchID || batches[0].Sequence != 1 {
				t.Fatal("committed batch lost or uncommitted batch became visible", err)
			}
			digest, err := agentwire.UsageDigest(batches[0])
			if err != nil || digest != saved.Digest {
				t.Fatal("committed batch contents changed during recovery", err)
			}
			start := time.Unix(1700000000, 0).UTC()
			if err = ledger.Sample(instance, start, start.Add(2*time.Second), []Counter{subject("crash-user", num(40), num(35))}); err != nil {
				t.Fatal("recovered cursor could not advance", err)
			}
			batches, err = ledger.Pending(10)
			if err != nil || len(batches) != 2 || batches[1].Sequence != 2 || batches[1].Items[0].UplinkBytes != 15 || batches[1].Items[0].DownlinkBytes != 20 {
				t.Fatal("cursor or sequence lost atomicity across crash", err)
			}
			if err = ledger.Ack(saved.Batch.BatchID, saved.Digest); err != nil {
				t.Fatal(err)
			}
			if count, _, err := ledger.Stats(); err != nil || count != 1 {
				t.Fatal("ack recovery changed unrelated durable evidence", count, err)
			}
		})
	}
}

func crashLedgerChild(t *testing.T, mode, path string) {
	t.Helper()
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// No defer Close: only SIGKILL is allowed to finish this helper.
	if err = ledger.BindIdentity("https://crash.test\x00crash-node"); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1700000000, 0).UTC()
	if err = ledger.Sample(instance, start, start.Add(time.Second), []Counter{subject("crash-user", num(25), num(15))}); err != nil {
		t.Fatal(err)
	}
	batches, err := ledger.Pending(1)
	if err != nil || len(batches) != 1 {
		t.Fatal("committed crash fixture absent", err)
	}
	digest, err := agentwire.UsageDigest(batches[0])
	if err != nil {
		t.Fatal(err)
	}
	if mode == "uncommitted" {
		if _, err = ledger.db.Exec(`PRAGMA cache_size=1;PRAGMA cache_spill=ON`); err != nil {
			t.Fatal(err)
		}
		tx, err := ledger.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`UPDATE cursors SET uplink=100,downlink=100;UPDATE instances SET sequence=2,last_sample=?`, start.Add(2*time.Second).UnixNano()); err != nil {
			t.Fatal(err)
		}
		phantom := batches[0]
		phantom.BatchID, phantom.Sequence = "uncommitted-batch", 2
		phantom.StartedAt, phantom.EndedAt = start.Add(time.Second), start.Add(2*time.Second)
		phantom.Items = []agentwire.UsageItem{{BindingID: "main", UserID: "crash-user", EntitlementID: "period-crash-user", UplinkBytes: 75, DownlinkBytes: 85}}
		payload, _ := json.Marshal(phantom)
		phantomDigest, _ := agentwire.UsageDigest(phantom)
		if _, err = tx.Exec(`INSERT INTO outbox(batch_id,instance_id,sequence,digest,payload) VALUES(?,?,?,?,?)`, phantom.BatchID, instance, phantom.Sequence, phantomDigest, payload); err != nil {
			t.Fatal(err)
		}
		// The transaction intentionally remains open while the parent kills us.
	} else if mode != "committed" {
		t.Fatal("invalid crash helper mode")
	}
	data, _ := json.Marshal(crashReady{Batch: batches[0], Digest: digest})
	fmt.Fprintln(os.Stdout, string(data))
	for {
		time.Sleep(time.Hour)
	}
}
