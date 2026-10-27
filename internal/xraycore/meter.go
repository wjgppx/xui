package xraycore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type TrafficCounter struct {
	BindingID, UserID, EntitlementID string
	Uplink, Downlink                 *int64
}
type Recorder func(instance string, startedAt, at time.Time, counters []TrafficCounter) error

var subjectID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SetRecorder must be called before serving traffic. Storage errors fail closed.
func (r *Runtime) SetRecorder(recorder Recorder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder = recorder
}

func parseCounters(data []byte) ([]TrafficCounter, error) {
	var response struct {
		Stat []struct {
			Name  string      `json:"name"`
			Value json.Number `json:"value"`
		} `json:"stat"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, errors.New("invalid statistics response")
	}
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("invalid statistics response")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("invalid statistics response")
	}
	subjects := map[string]*TrafficCounter{}
	seen := map[string]bool{}
	for _, stat := range response.Stat {
		parts := strings.Split(stat.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" || (parts[3] != "uplink" && parts[3] != "downlink") {
			return nil, errors.New("unexpected statistics counter")
		}
		ids := strings.Split(parts[1], ".")
		if len(ids) != 3 || !subjectID.MatchString(ids[0]) || !subjectID.MatchString(ids[1]) || !subjectID.MatchString(ids[2]) || seen[stat.Name] {
			return nil, errors.New("invalid statistics subject")
		}
		seen[stat.Name] = true
		value := int64(0)
		var err error
		if stat.Value != "" {
			value, err = stat.Value.Int64()
		}
		if err != nil || value < 0 {
			return nil, errors.New("invalid statistics value")
		}
		c := subjects[parts[1]]
		if c == nil {
			c = &TrafficCounter{BindingID: ids[0], UserID: ids[1], EntitlementID: ids[2]}
			subjects[parts[1]] = c
		}
		if parts[3] == "uplink" {
			c.Uplink = &value
		} else {
			c.Downlink = &value
		}
	}
	keys := make([]string, 0, len(subjects))
	for key := range subjects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	counters := make([]TrafficCounter, 0, len(keys))
	for _, key := range keys {
		counters = append(counters, *subjects[key])
	}
	return counters, nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > 1048576-b.Len() {
		return 0, errors.New("statistics response too large")
	}
	return b.Buffer.Write(p)
}
func (r *Runtime) sampleLocked(ctx context.Context) error {
	if r.recorder == nil || !r.aliveLocked() {
		return nil
	}
	sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(sampleCtx, r.binary, "api", "statsquery", "--server=127.0.0.1:"+strconv.Itoa(r.apiPort), "-pattern=user>>>")
	var output boundedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("statistics query failed")
	}
	counters, err := parseCounters(output.Bytes())
	if err != nil {
		return err
	}
	return r.recorder(r.instance, r.started, time.Now().UTC(), counters)
}

func (r *Runtime) Sample(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if err := r.sampleLocked(ctx); err != nil {
		r.stopLocked()
		return fail("meter_unavailable")
	}
	return nil
}
