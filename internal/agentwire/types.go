// Package agentwire defines the independently designed node control protocol.
package agentwire

import "time"

type EnrollmentRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	AgentToken      string `json:"agent_token"`
}
type EnrollmentResponse struct {
	NodeID string `json:"node_id"`
}
type DesiredState struct {
	SchemaVersion  int       `json:"schema_version"`
	NodeID         string    `json:"node_id"`
	Revision       int64     `json:"revision"`
	Bindings       []Binding `json:"bindings"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// BindingConfig contains references, never a TLS private key or proxy credential.
type BindingConfig struct {
	AccessMode  string   `json:"access_mode"`
	PublicHost  string   `json:"public_host"`
	PublicPort  int      `json:"public_port"`
	ID          string   `json:"id"`
	Protocol    string   `json:"protocol"`
	Listen      string   `json:"listen"`
	Port        int      `json:"port"`
	ServerName  string   `json:"server_name"`
	Certificate string   `json:"certificate"`
	Users       []string `json:"users"`
}
type Binding struct {
	ID          string      `json:"id"`
	Protocol    string      `json:"protocol"`
	Listen      string      `json:"listen"`
	Port        int         `json:"port"`
	ServerName  string      `json:"server_name"`
	Certificate string      `json:"certificate"`
	Clients     []ProxyUser `json:"clients"`
}
type ProxyUser struct {
	UserID        string    `json:"user_id"`
	EntitlementID string    `json:"entitlement_period_id"`
	Credential    string    `json:"credential"`
	ExpiresAt     time.Time `json:"expires_at"`
}
type AppliedState struct {
	Revision   int64  `json:"revision"`
	Status     string `json:"status"`
	ErrorCode  string `json:"error_code"`
	InstanceID string `json:"instance_id"`
}
type Heartbeat struct {
	AgentVersion     string `json:"agent_version"`
	ObservedRevision int64  `json:"observed_revision"`
}
