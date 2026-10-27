package agentwire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// UsageBatch is immutable after it enters the Agent's durable outbox. Sequence
// is scoped to a node/core instance; counters are deltas, never reset-on-read.
type UsageBatch struct {
	BatchID        string      `json:"batch_id"`
	CoreInstanceID string      `json:"core_instance_id"`
	Sequence       int64       `json:"sequence"`
	StartedAt      time.Time   `json:"started_at"`
	EndedAt        time.Time   `json:"ended_at"`
	Items          []UsageItem `json:"items"`
}
type UsageItem struct {
	BindingID     string `json:"binding_id"`
	UserID        string `json:"user_id"`
	EntitlementID string `json:"entitlement_period_id"`
	UplinkBytes   int64  `json:"uplink_bytes"`
	DownlinkBytes int64  `json:"downlink_bytes"`
}
type UsageAck struct {
	BatchID string `json:"batch_id"`
	Digest  string `json:"digest"`
}

// UsageDigest hashes the typed payload with UTC timestamps. It preserves item
// order, making any content/ordering change a different immutable batch.
func UsageDigest(batch UsageBatch) (string, error) {
	batch.StartedAt = batch.StartedAt.UTC()
	batch.EndedAt = batch.EndedAt.UTC()
	data, err := json.Marshal(batch)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
