// Package xraycore owns a separate Xray process and only accepts typed TLS bindings.
package xraycore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
)

type Failure struct{ Code string }

func (e *Failure) Error() string { return e.Code }
func fail(code string) error     { return &Failure{Code: code} }

type Runtime struct {
	apiPort                         int
	started                         time.Time
	recorder                        Recorder
	leaseMu                         sync.Mutex
	leasedProcess                   *os.Process
	mu                              sync.Mutex
	binary, directory, certificates string
	lock                            *os.File
	cmd                             *exec.Cmd
	done                            chan error
	active, digest, instance        string
	probes                          []probe
	lease                           time.Time
	timer                           *time.Timer
	closed                          bool
	// Only the build-tagged integration fixture sets a narrow local echo exception.
	testOutbound map[string]any
}
type probe struct {
	address, serverName string
	roots               *x509.CertPool
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var secretPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func New(binary, directory, certificates string) (*Runtime, error) {
	bin, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, errors.New("Xray executable unavailable")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runtime directory must be private (0700)")
	}
	lock, err := os.OpenFile(filepath.Join(directory, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("runtime already owned by another Agent")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		lock.Close()
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return &Runtime{binary: bin, directory: directory, certificates: certificates, lock: lock, apiPort: port}, nil
}

func (r *Runtime) render(d agentwire.DesiredState) ([]byte, []probe, string, error) {
	if len(d.Bindings) > 8 {
		return nil, nil, "", fail("config_rejected")
	}
	inbounds := []any{map[string]any{"tag": "xui-meter", "listen": "127.0.0.1", "port": r.apiPort, "protocol": "dokodemo-door", "settings": map[string]any{"address": "127.0.0.1"}}}
	probes := []probe{}
	ids, ports := map[string]bool{}, map[int]bool{}
	fingerprint := sha256.New()
	for _, b := range d.Bindings {
		if agentwire.ValidateBinding(b) != nil || b.Port == r.apiPort || ids[b.ID] || ports[b.Port] || b.Clients == nil || len(b.Clients) > 100 {
			return nil, nil, "", fail("config_rejected")
		}
		ids[b.ID], ports[b.Port] = true, true
		certPath := filepath.Join(r.certificates, b.Certificate+".crt")
		keyPath := filepath.Join(r.certificates, b.Certificate+".key")
		for _, path := range []string{certPath, keyPath} {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() > 1048576 || (path == keyPath && info.Mode().Perm()&0077 != 0) {
				return nil, nil, "", fail("config_rejected")
			}
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, nil, "", fail("config_rejected")
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil || leaf.VerifyHostname(b.ServerName) != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
			return nil, nil, "", fail("config_rejected")
		}
		roots := x509.NewCertPool()
		roots.AddCert(leaf)
		fingerprint.Write(cert.Certificate[0])
		clients := []any{}
		users := map[string]bool{}
		for _, u := range b.Clients {
			if !subjectID.MatchString(u.UserID) || !subjectID.MatchString(u.EntitlementID) || users[u.UserID] || u.ExpiresAt.Before(d.LeaseExpiresAt) {
				return nil, nil, "", fail("config_rejected")
			}
			users[u.UserID] = true
			c := map[string]any{"email": b.ID + "." + u.UserID + "." + u.EntitlementID, "level": 0}
			if b.Protocol == "vless" {
				if !uuidPattern.MatchString(u.Credential) {
					return nil, nil, "", fail("config_rejected")
				}
				c["id"] = u.Credential
			} else {
				if !secretPattern.MatchString(u.Credential) {
					return nil, nil, "", fail("config_rejected")
				}
				c["password"] = u.Credential
			}
			clients = append(clients, c)
		}
		settings := map[string]any{"clients": clients}
		if b.Protocol == "vless" {
			settings["decryption"] = "none"
		}
		inbounds = append(inbounds, map[string]any{"tag": b.ID, "listen": b.Listen, "port": b.Port, "protocol": b.Protocol, "settings": settings, "streamSettings": map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{"minVersion": "1.2", "alpn": []string{"http/1.1"}, "certificates": []any{map[string]any{"certificateFile": certPath, "keyFile": keyPath}}}}})
		host := b.Listen
		if net.ParseIP(host).IsUnspecified() {
			if net.ParseIP(host).To4() != nil {
				host = "127.0.0.1"
			} else {
				host = "::1"
			}
		}
		probes = append(probes, probe{net.JoinHostPort(host, strconv.Itoa(b.Port)), b.ServerName, roots})
	}
	outbound := map[string]any{}
	if r.testOutbound != nil {
		outbound = r.testOutbound
	}
	config := map[string]any{"api": map[string]any{"tag": "xui-meter", "services": []string{"StatsService"}}, "routing": map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": []string{"xui-meter"}, "outboundTag": "xui-meter"}}}, "log": map[string]any{"loglevel": "none"}, "inbounds": inbounds, "outbounds": []any{map[string]any{"protocol": "freedom", "settings": outbound}}, "stats": map[string]any{}, "policy": map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserUplink": true, "statsUserDownlink": true}}}}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, nil, "", fail("config_rejected")
	}
	fingerprint.Write(data)
	return data, probes, hex.EncodeToString(fingerprint.Sum(nil)), nil
}

func (r *Runtime) aliveLocked() bool {
	if r.cmd == nil {
		return false
	}
	select {
	case <-r.done:
		r.leaseMu.Lock()
		r.leasedProcess = nil
		r.leaseMu.Unlock()
		r.cmd = nil
		return false
	default:
		return true
	}
}
func (r *Runtime) stopLocked() {
	if r.cmd == nil {
		return
	}
	_ = r.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-r.done:
	case <-time.After(time.Second):
		_ = r.cmd.Process.Kill()
		<-r.done
	}
	r.leaseMu.Lock()
	r.leasedProcess = nil
	r.leaseMu.Unlock()
	r.cmd = nil
}
func (r *Runtime) leaseLocked(until time.Time) {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	r.lease = until
	if r.timer != nil {
		r.timer.Stop()
	}
	// Independent of the apply mutex: a slow validation/start cannot extend an
	// old authorization lease. Kill only the currently owned process.
	r.timer = time.AfterFunc(time.Until(until), func() {
		r.leaseMu.Lock()
		defer r.leaseMu.Unlock()
		if !time.Now().Before(r.lease) && r.leasedProcess != nil {
			_ = r.leasedProcess.Kill()
		}
	})
}
func (r *Runtime) startLocked(ctx context.Context, path string, probes []probe, lease time.Time) error {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return fail("start_failed")
	}
	cmd := exec.Command(r.binary, "run", "-config", path)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	// Do not leave an authorized proxy alive if the Agent is killed unexpectedly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	started := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		return fail("start_failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r.cmd, r.done = cmd, done
	r.instance, r.started = hex.EncodeToString(id), started
	r.leaseMu.Lock()
	r.leasedProcess = cmd.Process
	r.leaseMu.Unlock()
	r.leaseLocked(lease)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			r.stopLocked()
			return fail("start_failed")
		case <-time.After(100 * time.Millisecond):
		}
		if !r.aliveLocked() {
			return fail("start_failed")
		}
		ready := true
		for _, p := range probes {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 200 * time.Millisecond}, "tcp", p.address, &tls.Config{RootCAs: p.roots, ServerName: p.serverName, MinVersion: tls.VersionTLS12})
			if err != nil {
				ready = false
				break
			}
			conn.Close()
		}
		if ready && r.aliveLocked() {
			if r.recorder != nil {
				if err := r.sampleLocked(ctx); err != nil {
					r.stopLocked()
					return fail("meter_unavailable")
				}
			}
			return nil
		}
	}
	r.stopLocked()
	return fail("start_failed")
}

// Apply validates before replacing a process. An invalid replacement leaves the
// old process on its original lease; a failed start rolls back within that lease.
func (r *Runtime) Apply(ctx context.Context, d agentwire.DesiredState) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", fail("runtime_unavailable")
	}
	if !time.Now().Before(d.LeaseExpiresAt) || time.Until(d.LeaseExpiresAt) > 90*time.Second {
		return "", fail("lease_invalid")
	}
	data, probes, digest, err := r.render(d)
	if err != nil {
		return "", err
	}
	if digest == r.digest && (len(d.Bindings) == 0 || r.aliveLocked()) {
		r.leaseLocked(d.LeaseExpiresAt)
		return r.instance, nil
	}
	if len(d.Bindings) == 0 {
		if err := r.sampleLocked(ctx); err != nil {
			r.stopLocked()
			return "", fail("meter_unavailable")
		}
		r.stopLocked()
		if r.active != "" {
			os.Remove(r.active)
		}
		r.active = ""
		r.probes = nil
		r.digest = digest
		r.instance = "stopped"
		r.leaseLocked(d.LeaseExpiresAt)
		return r.instance, nil
	}
	f, err := os.CreateTemp(r.directory, "config-*.json")
	if err != nil {
		return "", fail("config_rejected")
	}
	candidate := f.Name()
	defer func() {
		if r.active != candidate {
			os.Remove(candidate)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return "", fail("config_rejected")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = exec.CommandContext(checkCtx, r.binary, "run", "-test", "-config", candidate).Run(); err != nil {
		return "", fail("config_rejected")
	}
	previous, previousLease, previousProbes := r.active, r.lease, r.probes
	if err := r.sampleLocked(ctx); err != nil {
		r.stopLocked()
		return "", fail("meter_unavailable")
	}
	r.stopLocked()
	if err = r.startLocked(ctx, candidate, probes, d.LeaseExpiresAt); err != nil {
		// Rollback probes use the old config's addresses and certificates.
		if previous != "" && time.Now().Before(previousLease) {
			_ = r.startLocked(ctx, previous, previousProbes, previousLease)
		}
		return "", err
	}
	if !time.Now().Before(d.LeaseExpiresAt) {
		r.stopLocked()
		return "", fail("lease_invalid")
	}
	if previous != "" {
		os.Remove(previous)
	}
	r.active, r.digest = candidate, digest
	r.probes = probes
	r.leaseLocked(d.LeaseExpiresAt)
	return r.instance, nil
}
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	finalErr := r.sampleLocked(context.Background())
	r.closed = true
	r.leaseMu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.leaseMu.Unlock()
	r.stopLocked()
	if r.active != "" {
		os.Remove(r.active)
	}
	_ = syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN)
	closeErr := r.lock.Close()
	if finalErr != nil {
		return fail("meter_unavailable")
	}
	return closeErr
}
