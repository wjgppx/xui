package xraycore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
)

// EnableHotAdd must run before the first Apply. The local Xray management API
// gains HandlerService, so this opt-in assumes the node's OS users are trusted.
func (r *Runtime) EnableHotAdd() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.digest != "" || r.cmd != nil {
		return errors.New("hot additions must be configured before applying a runtime")
	}
	r.hotAdd = true
	return nil
}

func copyBindings(bindings []agentwire.Binding) []agentwire.Binding {
	result := make([]agentwire.Binding, len(bindings))
	for i, binding := range bindings {
		result[i] = binding
		result[i].Clients = make([]agentwire.ProxyUser, len(binding.Clients))
		copy(result[i].Clients, binding.Clients)
		for j := range result[i].Clients {
			result[i].Clients[j].ExpiresAt = result[i].Clients[j].ExpiresAt.UTC()
		}
	}
	return result
}

// The structural fingerprint includes the full certificate/key material and
// every rendered setting except user accounts. It never leaves this runtime.
func renderShape(config map[string]any, material []byte) (string, error) {
	shape := make(map[string]any, len(config))
	for key, value := range config {
		shape[key] = value
	}
	var inbounds []any
	for _, raw := range config["inbounds"].([]any) {
		original := raw.(map[string]any)
		inbound := make(map[string]any, len(original))
		for key, value := range original {
			inbound[key] = value
		}
		if originalSettings, ok := original["settings"].(map[string]any); ok {
			settings := make(map[string]any, len(originalSettings))
			for key, value := range originalSettings {
				if key != "clients" {
					settings[key] = value
				}
			}
			inbound["settings"] = settings
		}
		inbounds = append(inbounds, inbound)
	}
	shape["inbounds"] = inbounds
	data, err := json.Marshal(shape)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write(material)
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sameUser(a, b agentwire.ProxyUser) bool {
	return a.UserID == b.UserID && a.EntitlementID == b.EntitlementID && a.Credential == b.Credential && a.ExpiresAt.Equal(b.ExpiresAt)
}

func hotAdditions(previous, next []agentwire.Binding) ([]agentwire.Binding, bool) {
	if len(previous) != len(next) || len(previous) == 0 {
		return nil, false
	}
	oldBindings := make(map[string]agentwire.Binding, len(previous))
	for _, binding := range previous {
		oldBindings[binding.ID] = binding
	}
	updates := []agentwire.Binding{}
	seenBindings := map[string]bool{}
	for _, binding := range next {
		old, exists := oldBindings[binding.ID]
		if !exists || seenBindings[binding.ID] || old.Protocol != binding.Protocol || old.Listen != binding.Listen || old.Port != binding.Port || old.ServerName != binding.ServerName || old.Certificate != binding.Certificate {
			return nil, false
		}
		seenBindings[binding.ID] = true
		newUsers := make(map[string]agentwire.ProxyUser, len(binding.Clients))
		for _, user := range binding.Clients {
			if _, duplicate := newUsers[user.UserID]; duplicate {
				return nil, false
			}
			newUsers[user.UserID] = user
		}
		oldUsers := make(map[string]bool, len(old.Clients))
		for _, user := range old.Clients {
			updated, exists := newUsers[user.UserID]
			if !exists || !sameUser(user, updated) {
				return nil, false
			}
			oldUsers[user.UserID] = true
		}
		update := binding
		update.Clients = []agentwire.ProxyUser{}
		for _, user := range binding.Clients {
			if !oldUsers[user.UserID] {
				update.Clients = append(update.Clients, user)
			}
		}
		if len(update.Clients) > 0 {
			updates = append(updates, update)
		}
	}
	return updates, true
}

func (r *Runtime) management(ctx context.Context, operation string, args ...string) ([]byte, error) {
	if r.hotCommand != nil {
		return r.hotCommand(ctx, operation, args...)
	}
	command := append([]string{"api", operation, "--server=127.0.0.1:" + strconv.Itoa(r.apiPort), "-t=2"}, args...)
	cmd := exec.CommandContext(ctx, r.binary, command...)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("runtime management unavailable")
	}
	return output.Bytes(), nil
}

func parseUserCount(data []byte) (int, error) {
	if len(data) > 4096 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return 0, errors.New("invalid runtime management response")
	}
	var result struct {
		Count json.RawMessage `json:"count"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return 0, errors.New("invalid runtime management response")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return 0, errors.New("invalid runtime management response")
	}
	// Protobuf JSON may omit its zero-valued count and encodes int64 as a string.
	if len(result.Count) == 0 {
		return 0, nil
	}
	value := string(result.Count)
	if strings.HasPrefix(value, "\"") {
		if err := json.Unmarshal(result.Count, &value); err != nil {
			return 0, errors.New("invalid runtime management count")
		}
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 || count > 100 || strconv.Itoa(count) != value {
		return 0, errors.New("invalid runtime management count")
	}
	return count, nil
}

func (r *Runtime) verifyUserCounts(ctx context.Context, bindings []agentwire.Binding) error {
	for _, binding := range bindings {
		data, err := r.management(ctx, "inboundusercount", "-tag="+binding.ID)
		if err != nil {
			return err
		}
		count, err := parseUserCount(data)
		if err != nil || count != len(binding.Clients) {
			return errors.New("runtime user count mismatch")
		}
	}
	return nil
}

func (r *Runtime) hotPatch(additions []agentwire.Binding) (string, error) {
	inbounds := []any{}
	for _, binding := range additions {
		clients := []any{}
		for _, user := range binding.Clients {
			client := map[string]any{"email": binding.ID + "." + user.UserID + "." + user.EntitlementID, "level": 0}
			if binding.Protocol == "vless" {
				client["id"] = user.Credential
			} else {
				client["password"] = user.Credential
			}
			clients = append(clients, client)
		}
		settings := map[string]any{"clients": clients}
		if binding.Protocol == "vless" {
			settings["decryption"] = "none"
		}
		inbounds = append(inbounds, map[string]any{"tag": binding.ID, "listen": binding.Listen, "port": binding.Port, "protocol": binding.Protocol, "settings": settings})
	}
	data, err := json.Marshal(map[string]any{"inbounds": inbounds})
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(r.directory, "hot-users-*.json")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		os.Remove(name)
		return "", errors.New("runtime patch unavailable")
	}
	return name, nil
}

func (r *Runtime) stopHotFailureLocked(code string) error {
	// Removing users cannot revoke their already-open Xray connections. Stop
	// the whole process after any uncertain partial update, keeping old config
	// metadata for a full, freshly authorized retry on the next Apply.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := r.sampleLocked(ctx)
	cancel()
	r.stopLocked()
	if err != nil {
		return fail("meter_unavailable")
	}
	return fail(code)
}

// Extend a successful update atomically against the autonomous expiry callback.
func (r *Runtime) commitHotLeaseLocked(until time.Time) bool {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	if r.leasedProcess == nil || !time.Now().Before(r.lease) || !time.Now().Before(until) {
		return false
	}
	r.installLeaseTimerLocked(until)
	return true
}

func (r *Runtime) hotUpdateLocked(ctx context.Context, desired agentwire.DesiredState, additions []agentwire.Binding) error {
	patch := ""
	if len(additions) > 0 {
		var err error
		patch, err = r.hotPatch(additions)
		if err != nil {
			return fail("config_rejected")
		}
		defer os.Remove(patch)
	}
	deadline := r.lease
	if desired.LeaseExpiresAt.Before(deadline) {
		deadline = desired.LeaseExpiresAt
		// Shortening is safe immediately; extending is deferred until verified.
		r.leaseLocked(deadline)
	}
	operationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	operationCtx, deadlineCancel := context.WithDeadline(operationCtx, deadline)
	defer deadlineCancel()
	failure := func() error {
		if !time.Now().Before(deadline) {
			return r.stopHotFailureLocked("lease_invalid")
		}
		return r.stopHotFailureLocked("runtime_unavailable")
	}
	if err := r.sampleLocked(operationCtx); err != nil {
		return r.stopHotFailureLocked("meter_unavailable")
	}
	if err := r.verifyUserCounts(operationCtx, r.bindings); err != nil {
		return failure()
	}
	if patch != "" {
		if _, err := r.management(operationCtx, "adu", patch); err != nil {
			return failure()
		}
	}
	if err := r.verifyUserCounts(operationCtx, desired.Bindings); err != nil {
		return failure()
	}
	if err := operationCtx.Err(); err != nil {
		return failure()
	}
	if err := r.sampleLocked(operationCtx); err != nil {
		return r.stopHotFailureLocked("meter_unavailable")
	}
	if !r.aliveLocked() || !r.commitHotLeaseLocked(desired.LeaseExpiresAt) {
		return failure()
	}
	return nil
}
