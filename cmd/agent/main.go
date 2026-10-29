package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/usageledger"
	"github.com/wjgppx/xui/internal/xraycore"
)

func main() { os.Exit(run()) }
func run() int {
	hotEnvironment := os.Getenv("XUI_HOT_ADD_USERS")
	if hotEnvironment != "" && hotEnvironment != "true" && hotEnvironment != "false" {
		slog.Error("XUI_HOT_ADD_USERS must be true or false")
		return 1
	}
	controller := flag.String("controller", "", "Control plane HTTPS origin (loopback HTTP allowed for development)")
	state := flag.String("state", ".local/agent/identity.json", "Private identity state path")
	once := flag.Bool("once", false, "Observe configuration and report once; no persistent proxy service")
	binary := flag.String("xray", "", "Xray executable; omit for observation-only mode")
	certificates := flag.String("certificates", "", "Directory containing <reference>.crt and private <reference>.key")
	hotAddUsers := flag.Bool("hot-add-users", hotEnvironment == "true", "Opt in to local HandlerService for add-only users without restarting Xray")
	flag.Parse()
	if *hotAddUsers && *binary == "" {
		slog.Error("--hot-add-users requires --xray")
		return 1
	}
	if *once && *binary != "" {
		slog.Error("--once cannot own a persistent Xray service")
		return 1
	}
	c, err := nodeagent.New(*state, *controller, os.Getenv("XUI_ENROLL_TOKEN"))
	_ = os.Unsetenv("XUI_ENROLL_TOKEN")
	if err != nil {
		slog.Error("agent initialization failed", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *binary != "" {
		if *certificates == "" {
			slog.Error("--certificates is required with --xray")
			return 1
		}
		ledger, err := usageledger.Open(filepath.Join(filepath.Dir(*state), "usage", "ledger.sqlite"))
		if err != nil {
			slog.Error("usage storage unavailable")
			return 1
		}
		defer ledger.Close()
		runtime, err := xraycore.New(*binary, filepath.Join(filepath.Dir(*state), "runtime"), *certificates)
		if err != nil {
			slog.Error("runtime initialization failed", "error", err)
			return 1
		}
		if *hotAddUsers {
			if err = runtime.EnableHotAdd(); err != nil {
				_ = runtime.Close()
				slog.Error("hot user update configuration invalid")
				return 1
			}
		}
		c.Outbox = ledger
		c.Runtime = runtime
		runtime.SetRecorder(func(instance string, start, at time.Time, observations []xraycore.TrafficCounter) error {
			counters := make([]usageledger.Counter, 0, len(observations))
			for _, v := range observations {
				counters = append(counters, usageledger.Counter{BindingID: v.BindingID, UserID: v.UserID, EntitlementID: v.EntitlementID, Uplink: v.Uplink, Downlink: v.Downlink})
			}
			return ledger.Sample(instance, start, at, counters)
		})
		// The sampling goroutine is independent of controller retries/backoff.
		sampleCtx, cancelSampling := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-sampleCtx.Done():
					return
				case <-ticker.C:
					if err := runtime.Sample(sampleCtx); err != nil {
						slog.Warn("traffic sampling failed; proxy stopped")
					}
				}
			}
		}()
		defer func() {
			cancelSampling()
			<-done
			if err := runtime.Close(); err != nil {
				slog.Warn("final traffic sample failed")
			}
			finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.FlushUsage(finalCtx); err != nil {
				slog.Warn("usage retained for next start")
			}
		}()
	}
	delay := 10 * time.Second
	for {
		err = c.Sync(ctx)
		if ctx.Err() != nil {
			return 0
		}
		if err != nil {
			slog.Warn("node sync failed", "error", err)
			if *once || errors.Is(err, nodeagent.ErrIdentity) {
				return 1
			}
			delay *= 2
			if delay > time.Minute {
				delay = time.Minute
			}
		} else {
			slog.Info("node sync accepted", "node_id", c.State.NodeID, "observed_revision", c.State.ObservedRevision, "runtime_enabled", c.Runtime != nil, "hot_add_users_enabled", *hotAddUsers)
			if *once {
				return 0
			}
			delay = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(delay):
		}
	}
}
