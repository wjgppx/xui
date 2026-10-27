package httpapi

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
)

type node struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Enrolled     bool       `json:"enrolled"`
	Revoked      bool       `json:"revoked"`
	Desired      int64      `json:"desired_revision"`
	Observed     int64      `json:"observed_revision"`
	AgentVersion string     `json:"agent_version"`
	LastSeen     *time.Time `json:"last_seen_at"`
	CreatedAt    time.Time  `json:"created_at"`
	Applied      int64      `json:"applied_revision"`
	ApplyStatus  string     `json:"apply_status"`
	ApplyError   string     `json:"apply_error"`
	CoreInstance string     `json:"core_instance_id"`
	AppliedAt    *time.Time `json:"applied_reported_at"`
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	rows, err := s.Store.DB.Query(r.Context(), `SELECT id,name,agent_token_hash IS NOT NULL,revoked_at IS NOT NULL,desired_revision,observed_revision,agent_version,last_seen_at,created_at,applied_revision,apply_status,apply_error,core_instance_id,applied_reported_at FROM nodes ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []node{}
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.ID, &n.Name, &n.Enrolled, &n.Revoked, &n.Desired, &n.Observed, &n.AgentVersion, &n.LastSeen, &n.CreatedAt, &n.Applied, &n.ApplyStatus, &n.ApplyError, &n.CoreInstance, &n.AppliedAt); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, n)
	}
	if err = rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, out)
}
func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		fail(w, 400, "节点名称需为 1–100 字节")
		return
	}
	id, token, expires := store.ID(), store.ID()+store.ID(), time.Now().Add(15*time.Minute)
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `INSERT INTO nodes(id,name,enrollment_hash,enrollment_expires_at) VALUES($1,$2,$3,$4)`, id, body.Name, hash(token), expires)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'node.created',$2)`, u.ID, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 201, map[string]any{"node_id": id, "enrollment_token": token, "expires_at": expires})
}
func (s *Server) rotateNode(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	token := store.ID() + store.ID()
	expires := time.Now().Add(15 * time.Minute)
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE nodes SET enrollment_hash=$1,enrollment_expires_at=$2,agent_token_hash=NULL,enrolled_at=NULL,revoked_at=NULL,observed_revision=0,last_seen_at=NULL,agent_version='',applied_revision=0,apply_status='pending',apply_error='',core_instance_id='',applied_reported_at=NULL WHERE id=$3`, hash(token), expires, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, 404, "节点不存在")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'node.identity_rotated',$2)`, u.ID, id)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]any{"node_id": id, "enrollment_token": token, "expires_at": expires})
}
func (s *Server) revokeNode(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	tag, err := tx.Exec(r.Context(), `UPDATE nodes SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, 404, "节点不存在")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'node.revoked',$2)`, u.ID, id)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	if !s.allowed(r) {
		fail(w, 429, "请求过于频繁")
		return
	}
	var body agentwire.EnrollmentRequest
	if !decode(w, r, &body) {
		return
	}
	if !validToken(body.EnrollmentToken) || !validToken(body.AgentToken) {
		fail(w, 400, "节点令牌格式不正确")
		return
	}
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	var previous *string
	err = tx.QueryRow(r.Context(), `SELECT id,agent_token_hash FROM nodes WHERE enrollment_hash=$1 AND enrollment_expires_at>now() AND revoked_at IS NULL FOR UPDATE`, hash(body.EnrollmentToken)).Scan(&id, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "注册凭据无效或已过期")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	credentialHash := hash(body.AgentToken)
	// The agent persists its credential BEFORE enrolling. Retrying a lost response
	// is allowed only for that same credential; another instance cannot reuse it.
	if previous != nil && subtle.ConstantTimeCompare([]byte(*previous), []byte(credentialHash)) != 1 {
		fail(w, 409, "注册凭据已被使用")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE nodes SET agent_token_hash=$1,enrolled_at=COALESCE(enrolled_at,now()) WHERE id=$2`, credentialHash, id)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, agentwire.EnrollmentResponse{NodeID: id})
}
func (s *Server) agentIdentity(w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || !validToken(token) {
		fail(w, 401, "需要有效节点身份")
		return "", 0, false
	}
	var id string
	var revision int64
	err := s.Store.DB.QueryRow(r.Context(), `SELECT id,desired_revision FROM nodes WHERE agent_token_hash=$1 AND revoked_at IS NULL`, hash(token)).Scan(&id, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "节点身份失效")
		return "", 0, false
	}
	if err != nil {
		serverError(w, err)
		return "", 0, false
	}
	return id, revision, true
}
func (s *Server) desiredState(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !validToken(token) {
		fail(w, 401, "需要有效节点身份")
		return
	}
	state, err := s.snapshot(r.Context(), token)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "节点身份失效")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, state)
}
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentIdentity(w, r)
	if !ok {
		return
	}
	var body agentwire.Heartbeat
	if !decode(w, r, &body) {
		return
	}
	if body.AgentVersion == "" || len(body.AgentVersion) > 64 || body.ObservedRevision < 0 {
		fail(w, 400, "心跳字段不正确")
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	tag, err := s.Store.DB.Exec(r.Context(), `UPDATE nodes SET agent_version=$1,observed_revision=$2,last_seen_at=now() WHERE id=$3 AND agent_token_hash=$4 AND revoked_at IS NULL AND $2>=observed_revision AND $2<=desired_revision`, body.AgentVersion, body.ObservedRevision, id, hash(token))
	if err != nil {
		serverError(w, err)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, 409, "版本落后、超前或节点身份已撤销")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
