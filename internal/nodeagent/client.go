package nodeagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/xraycore"
)

const Version = "0.4.0"

var ErrIdentity = errors.New("node identity rejected; ask an administrator to issue a new enrollment")

type State struct {
	Controller       string `json:"controller"`
	NodeID           string `json:"node_id"`
	AgentToken       string `json:"agent_token"`
	EnrollmentToken  string `json:"enrollment_token,omitempty"`
	ObservedRevision int64  `json:"observed_revision"`
}
type Applier interface {
	Apply(context.Context, agentwire.DesiredState) (string, error)
}

type Client struct {
	Outbox  UsageOutbox
	Runtime Applier
	HTTP    *http.Client
	State   State
	Path    string
}

func New(path, controller, enrollment string) (*Client, error) {
	c := &Client{Path: path, HTTP: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("agent state must be a regular private file (0600)")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &c.State); err != nil {
			return nil, errors.New("invalid agent state")
		}
		if controller != "" && controller != c.State.Controller {
			return nil, errors.New("controller differs from saved identity; use a separate state file")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if !validToken(enrollment) {
			return nil, errors.New("first run requires a 64-character hex XUI_ENROLL_TOKEN")
		}
		b := make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		c.State = State{Controller: controller, AgentToken: hex.EncodeToString(b), EnrollmentToken: enrollment}
		if err = validateController(controller); err != nil {
			return nil, err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		data, _ := json.Marshal(c.State)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		err = directory.Sync()
		_ = directory.Close()
		if err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if err = validateController(c.State.Controller); err != nil {
		return nil, err
	}
	if !validToken(c.State.AgentToken) || c.State.NodeID == "" && !validToken(c.State.EnrollmentToken) {
		return nil, errors.New("invalid saved node credential")
	}
	return c, nil
}
func validToken(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func validateController(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("controller must be a single HTTPS origin")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("HTTPS required except literal loopback addresses for local development")
}
func (c *Client) save() error {
	data, err := json.Marshal(c.State)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.Path), ".xui-agent-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, c.Path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(c.Path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.State.Controller+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if path != "/agent/v1/enroll" {
		req.Header.Set("Authorization", "Bearer "+c.State.AgentToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("controller request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrIdentity
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("controller returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if err != nil {
		return err
	}
	if len(data) > 1048576 {
		return errors.New("controller response too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(out); err != nil {
		return errors.New("invalid controller response")
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("invalid controller response")
	}
	return nil
}
func (c *Client) Sync(ctx context.Context) error {
	if c.State.NodeID == "" {
		var result agentwire.EnrollmentResponse
		err := c.request(ctx, "POST", "/agent/v1/enroll", agentwire.EnrollmentRequest{EnrollmentToken: c.State.EnrollmentToken, AgentToken: c.State.AgentToken}, &result)
		if err != nil {
			return err
		}
		if strings.TrimSpace(result.NodeID) == "" {
			return errors.New("empty node identity")
		}
		c.State.NodeID = result.NodeID
		c.State.EnrollmentToken = ""
		if err = c.save(); err != nil {
			return err
		}
	}
	if err := c.bindUsageIdentity(); err != nil {
		return err
	}
	if sampler, ok := c.Runtime.(interface{ Sample(context.Context) error }); ok {
		if err := sampler.Sample(ctx); err != nil {
			if c.State.ObservedRevision > 0 {
				_ = c.request(ctx, "POST", "/agent/v1/applied-state", agentwire.AppliedState{Revision: c.State.ObservedRevision, Status: "failed", ErrorCode: "meter_unavailable"}, nil)
			}
			return err
		}
	}
	if err := c.FlushUsage(ctx); err != nil {
		return err
	}
	var desired agentwire.DesiredState
	if err := c.request(ctx, "GET", "/agent/v1/desired-state", nil, &desired); err != nil {
		return err
	}
	if desired.SchemaVersion != 1 || desired.NodeID != c.State.NodeID || desired.Revision < 1 || desired.Revision < c.State.ObservedRevision || desired.Bindings == nil {
		return errors.New("invalid or regressing desired state")
	}
	if len(desired.Bindings) > 0 && c.Runtime == nil {
		return errors.New("proxy configuration requires an Xray runtime")
	}
	c.State.ObservedRevision = desired.Revision
	if err := c.save(); err != nil {
		return err
	}
	if err := c.request(ctx, "POST", "/agent/v1/heartbeat", agentwire.Heartbeat{AgentVersion: Version, ObservedRevision: desired.Revision}, nil); err != nil {
		return err
	}
	if c.Runtime == nil {
		return nil
	}
	instance, applyErr := c.Runtime.Apply(ctx, desired)
	report := agentwire.AppliedState{Revision: desired.Revision, Status: "applied", InstanceID: instance}
	if applyErr != nil {
		report.Status = "failed"
		report.InstanceID = ""
		report.ErrorCode = "runtime_unavailable"
		var failure *xraycore.Failure
		if errors.As(applyErr, &failure) {
			report.ErrorCode = failure.Code
		}
	}
	if err := c.request(ctx, "POST", "/agent/v1/applied-state", report, nil); err != nil {
		return err
	}
	return applyErr
}
