package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/agentwire"
)

type nodeConfiguration struct {
	Revision int64                     `json:"revision"`
	Bindings []agentwire.BindingConfig `json:"bindings"`
}

func (s *Server) configuration(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	var data []byte
	var cfg nodeConfiguration
	err := s.Store.DB.QueryRow(r.Context(), `SELECT configuration,desired_revision FROM nodes WHERE id=$1`, r.PathValue("id")).Scan(&data, &cfg.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "节点不存在")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	revision := cfg.Revision
	if err = json.Unmarshal(data, &cfg); err != nil {
		serverError(w, err)
		return
	}
	cfg.Revision = revision
	reply(w, 200, cfg)
}
func (s *Server) configure(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.current(w, r, true)
	if !ok {
		return
	}
	var cfg nodeConfiguration
	if !decode(w, r, &cfg) {
		return
	}
	if cfg.Revision < 1 || cfg.Bindings == nil || len(cfg.Bindings) > 8 {
		fail(w, 400, "需要当前版本和最多 8 个入站")
		return
	}
	ids, ports := map[string]bool{}, map[int]bool{}
	for i := range cfg.Bindings {
		b := &cfg.Bindings[i]
		if b.AccessMode == "" {
			b.AccessMode = "manual"
		}
		if (b.AccessMode != "manual" && b.AccessMode != "plan") || (b.AccessMode == "plan" && len(b.Users) > 0) {
			fail(w, 400, "授权模式不正确；套餐模式不接受逐用户名单")
			return
		}
		if err := agentwire.ValidateBinding(agentwire.Binding{ID: b.ID, Protocol: b.Protocol, Listen: b.Listen, Port: b.Port, ServerName: b.ServerName, Certificate: b.Certificate}); err != nil {
			fail(w, 400, err.Error())
			return
		}
		if !validPublication(*b) {
			fail(w, 400, "订阅连接地址和端口格式不正确")
			return
		}
		if ids[b.ID] || ports[b.Port] || b.Users == nil || len(b.Users) > 100 {
			fail(w, 400, "入站 ID/端口不可重复，每个入站最多 100 个用户")
			return
		}
		ids[b.ID], ports[b.Port] = true, true
		seen := map[string]bool{}
		for j, email := range b.Users {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" || seen[email] {
				fail(w, 400, "授权用户重复或为空")
				return
			}
			seen[email] = true
			b.Users[j] = email
		}
		sort.Strings(b.Users)
	}
	sort.Slice(cfg.Bindings, func(i, j int) bool { return cfg.Bindings[i].ID < cfg.Bindings[j].ID })
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var revision int64
	err = tx.QueryRow(r.Context(), `SELECT desired_revision FROM nodes WHERE id=$1 FOR UPDATE`, r.PathValue("id")).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "节点不存在")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if revision != cfg.Revision {
		fail(w, 409, "配置已更新，请重新读取后编辑")
		return
	}
	for _, b := range cfg.Bindings {
		for _, email := range b.Users {
			var userID string
			err = tx.QueryRow(r.Context(), `SELECT id FROM users WHERE email=$1`, email).Scan(&userID)
			if errors.Is(err, pgx.ErrNoRows) {
				fail(w, 400, "授权列表含未注册用户")
				return
			}
			if err != nil {
				serverError(w, err)
				return
			}
			_, _, err = s.Store.EnsureNodeGrant(r.Context(), tx, r.PathValue("id"), userID)
			if err != nil {
				serverError(w, err)
				return
			}
		}
	}
	cfg.Revision = revision + 1
	data, _ := json.Marshal(cfg)
	_, err = tx.Exec(r.Context(), `UPDATE nodes SET configuration=$1,desired_revision=$2,desired_digest='',apply_status='pending',apply_error='' WHERE id=$3`, data, cfg.Revision, r.PathValue("id"))
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'node.configured',$2)`, admin.ID, r.PathValue("id"))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, cfg)
}

// Snapshot and digest are serialized with configuration writes. Time-based
// entitlement transitions advance revisions even without an outbox consumer.
func (s *Server) snapshot(ctx context.Context, token string) (agentwire.DesiredState, error) {
	tx, err := s.Store.DB.Begin(ctx)
	if err != nil {
		return agentwire.DesiredState{}, err
	}
	defer tx.Rollback(ctx)
	var out agentwire.DesiredState
	var data []byte
	var digest string
	err = tx.QueryRow(ctx, `SELECT id,desired_revision,configuration,desired_digest FROM nodes WHERE agent_token_hash=$1 AND revoked_at IS NULL FOR UPDATE`, hash(token)).Scan(&out.NodeID, &out.Revision, &data, &digest)
	if err != nil {
		return out, err
	}
	var cfg nodeConfiguration
	if err = json.Unmarshal(data, &cfg); err != nil {
		return out, err
	}
	now := time.Now().UTC()
	out.SchemaVersion = 1
	out.LeaseExpiresAt = now.Add(time.Minute)
	out.Bindings = []agentwire.Binding{}
	for _, definition := range cfg.Bindings {
		b := agentwire.Binding{ID: definition.ID, Protocol: definition.Protocol, Listen: definition.Listen, Port: definition.Port, ServerName: definition.ServerName, Certificate: definition.Certificate, Clients: []agentwire.ProxyUser{}}
		subjects, err := s.Store.BindingSubjects(ctx, tx, out.NodeID, definition.AccessMode, definition.Users, now)
		if err != nil {
			return out, err
		}
		for _, subject := range subjects {
			uuid, secret, err := s.Store.EnsureNodeGrant(ctx, tx, out.NodeID, subject.UserID)
			if err != nil {
				return out, err
			}
			c := agentwire.ProxyUser{UserID: subject.UserID, EntitlementID: subject.EntitlementID, ExpiresAt: subject.ExpiresAt, Credential: uuid}
			if b.Protocol == "trojan" {
				c.Credential = secret
			}
			if c.ExpiresAt.Before(out.LeaseExpiresAt) {
				out.LeaseExpiresAt = c.ExpiresAt
			}
			b.Clients = append(b.Clients, c)
		}
		out.Bindings = append(out.Bindings, b)
	}
	for _, binding := range out.Bindings {
		for _, client := range binding.Clients {
			_, err = tx.Exec(ctx, `WITH renewed AS (UPDATE node_usage_authorizations SET authorized_until=GREATEST(authorized_until,$6) WHERE node_id=$1 AND binding_id=$2 AND user_id=$3 AND entitlement_period_id=$4 AND authorized_until >= $5 RETURNING 1) INSERT INTO node_usage_authorizations(node_id,binding_id,user_id,entitlement_period_id,authorized_from,authorized_until) SELECT $1,$2,$3,$4,$5,$6 WHERE NOT EXISTS(SELECT 1 FROM renewed)`, out.NodeID, binding.ID, client.UserID, client.EntitlementID, now, out.LeaseExpiresAt)
			if err != nil {
				return out, err
			}
		}
	}
	payload, _ := json.Marshal(out.Bindings)
	next := hash(string(payload))
	if digest != "" && digest != next {
		out.Revision++
	}
	_, err = tx.Exec(ctx, `UPDATE nodes SET desired_revision=$1,desired_digest=$2,apply_status=CASE WHEN desired_revision<>$1 THEN 'pending' ELSE apply_status END,apply_error=CASE WHEN desired_revision<>$1 THEN '' ELSE apply_error END WHERE id=$3`, out.Revision, next, out.NodeID)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return out, err
}

func (s *Server) applied(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentIdentity(w, r)
	if !ok {
		return
	}
	var body agentwire.AppliedState
	if !decode(w, r, &body) {
		return
	}
	codes := map[string]bool{"config_rejected": true, "start_failed": true, "lease_invalid": true, "runtime_unavailable": true, "meter_unavailable": true}
	if body.Revision < 1 || (body.Status != "applied" && body.Status != "failed") || (body.Status == "applied" && (body.ErrorCode != "" || !agentwire.NamePattern.MatchString(body.InstanceID))) || (body.Status == "failed" && (!codes[body.ErrorCode] || body.InstanceID != "")) {
		fail(w, 400, "应用结果字段不正确")
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	tag, err := s.Store.DB.Exec(r.Context(), `UPDATE nodes SET applied_revision=CASE WHEN $1='applied' THEN $2 ELSE applied_revision END,apply_status=$1,apply_error=$3,core_instance_id=$4,applied_reported_at=now() WHERE id=$5 AND agent_token_hash=$6 AND revoked_at IS NULL AND desired_revision=$2 AND observed_revision>=$2`, body.Status, body.Revision, body.ErrorCode, body.InstanceID, id, hash(token))
	if err != nil {
		serverError(w, err)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, 409, "配置版本或节点身份已变化")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
