//go:build xrayintegration

package xraycore

import "time"

// AllowLoopbackEcho exists only in the integration-test build. Production builds
// preserve Xray's default private-destination restrictions without an override.
func (r *Runtime) AllowLoopbackEcho(port int) {
	r.testOutbound = map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1/32"}, "port": port}}}
}

func (r *Runtime) IntegrationLeaseProbe() (time.Time, int) {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	pid := 0
	if r.leasedProcess != nil {
		pid = r.leasedProcess.Pid
	}
	return r.lease, pid
}
