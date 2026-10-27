//go:build xrayintegration

package integration_test

import (
	"io"
	"net/http"
	"strings"
)

// The real server commits; only the first acknowledgement is replaced with a
// retryable failure to model a lost reply across an uncertain network boundary.
type loseUsageAck struct {
	base http.RoundTripper
	lost bool
}

func (l *loseUsageAck) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := l.base.RoundTrip(req)
	if err == nil && !l.lost && req.URL.Path == "/agent/v1/usage-batches" && response.StatusCode == 200 {
		l.lost = true
		response.Body.Close()
		response.StatusCode = 503
		response.Body = io.NopCloser(strings.NewReader(`{"error":"simulated lost acknowledgement"}`))
	}
	return response, err
}
