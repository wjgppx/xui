package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/subscription"
)

func (s *Server) subscriptionStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	var created *time.Time
	var enabled bool
	err := s.Store.DB.QueryRow(r.Context(), `SELECT token_hash IS NOT NULL,created_at FROM subscriptions WHERE user_id=$1`, u.ID).Scan(&enabled, &created)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]any{"enabled": enabled, "created_at": created})
}
func (s *Server) rotateSubscription(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	token := store.ID() + store.ID()
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `INSERT INTO subscriptions(user_id,token_hash) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET token_hash=EXCLUDED.token_hash,created_at=now()`, u.ID, hash(token))
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'subscription.rotated',$1)`, u.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	urls := map[string]string{}
	for _, format := range []string{"uri", "base64", "clash", "xray"} {
		urls[format] = s.Config.Origin + "/api/v1/subscriptions/" + token + "?format=" + format
	}
	reply(w, 200, map[string]any{"token": token, "urls": urls})
}
func (s *Server) revokeSubscription(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `UPDATE subscriptions SET token_hash=NULL WHERE user_id=$1`, u.ID)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'subscription.revoked',$1)`, u.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Server) rotateProxyCredentials(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// Use the same node-first lock order as configuration/snapshot writes. Lock all
	// target nodes before changing grants, so a subscription never mixes generations.
	rows, err := tx.Query(r.Context(), `SELECT n.id FROM nodes n WHERE EXISTS(SELECT 1 FROM node_grants g WHERE g.node_id=n.id AND g.user_id=$1) ORDER BY n.id FOR UPDATE`, u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		serverError(w, err)
		return
	}
	for _, id := range ids {
		raw := store.ID()
		uuid := fmt.Sprintf("%s-%s-%s-%s-%s", raw[:8], raw[8:12], raw[12:16], raw[16:20], raw[20:])
		_, err = tx.Exec(r.Context(), `UPDATE node_grants SET credential_id=$1,secret=$2 WHERE node_id=$3 AND user_id=$4`, uuid, store.ID()+store.ID(), id, u.ID)
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE nodes SET desired_revision=desired_revision+1,desired_digest='',apply_status='pending',apply_error='' WHERE id=$1`, id)
		}
		if err != nil {
			serverError(w, err)
			return
		}
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'proxy_credentials.rotated',$1)`, u.ID)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *Server) publicSubscription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if !s.allowedKey(r, "subscription:", 60) {
		fail(w, 429, "请求过于频繁")
		return
	}
	token := r.PathValue("token")
	if !validToken(token) {
		fail(w, 401, "订阅链接无效")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "base64"
	}
	if format != "uri" && format != "base64" && format != "clash" && format != "xray" {
		fail(w, 400, "不支持的订阅格式")
		return
	}
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var userID string
	// A completed revoke/rotation must invalidate subsequent reads. Lock the token
	// while rendering; the writer waits for any already authorized in-flight read.
	err = tx.QueryRow(r.Context(), `SELECT user_id FROM subscriptions WHERE token_hash=$1 FOR SHARE`, hash(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "订阅链接无效")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	var eligible bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM entitlement_periods WHERE user_id=$1 AND starts_at<=now() AND ends_at>now() AND used_bytes<quota_bytes AND revoked_at IS NULL)`, userID).Scan(&eligible)
	if err != nil {
		serverError(w, err)
		return
	}
	endpoints := []subscription.Endpoint{}
	if eligible {
		rows, err := tx.Query(r.Context(), `SELECT n.id,n.name,n.configuration,g.credential_id,g.secret FROM nodes n JOIN node_grants g ON g.node_id=n.id AND g.user_id=$1 WHERE n.revoked_at IS NULL AND n.agent_token_hash IS NOT NULL AND n.last_seen_at>now()-interval '60 seconds' AND n.apply_status='applied' AND n.applied_revision=n.desired_revision ORDER BY n.id FOR SHARE OF n`, userID)
		if err != nil {
			serverError(w, err)
			return
		}
		type grantedNode struct {
			id, name, uuid, secret string
			data                   []byte
		}
		nodes := []grantedNode{}
		for rows.Next() {
			var n grantedNode
			if err = rows.Scan(&n.id, &n.name, &n.data, &n.uuid, &n.secret); err != nil {
				rows.Close()
				serverError(w, err)
				return
			}
			nodes = append(nodes, n)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			serverError(w, err)
			return
		}
		// Grants can change while acquiring a node lock; read credentials again after
		// all node locks are held, so only the serialized generation is exported.

		for _, n := range nodes {
			if err = tx.QueryRow(r.Context(), `SELECT credential_id,secret FROM node_grants WHERE node_id=$1 AND user_id=$2`, n.id, userID).Scan(&n.uuid, &n.secret); err != nil {
				serverError(w, err)
				return
			}
			var cfg nodeConfiguration
			if err = json.Unmarshal(n.data, &cfg); err != nil {
				serverError(w, err)
				return
			}
			for _, b := range cfg.Bindings {
				if b.PublicHost == "" || b.PublicPort == 0 {
					continue
				}
				subjects, err := s.Store.BindingSubjects(r.Context(), tx, n.id, b.AccessMode, b.Users, time.Now().UTC())
				if err != nil {
					serverError(w, err)
					return
				}
				periodID := ""
				for _, subject := range subjects {
					if subject.UserID == userID {
						periodID = subject.EntitlementID
						break
					}
				}
				if periodID == "" {
					continue
				}
				var issued bool
				err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM node_usage_authorizations a JOIN entitlement_periods e ON e.id=a.entitlement_period_id AND e.user_id=a.user_id WHERE a.node_id=$1 AND a.binding_id=$2 AND a.user_id=$3 AND a.entitlement_period_id=$4 AND a.authorized_until>now() AND e.revoked_at IS NULL AND e.starts_at<=now() AND e.ends_at>now() AND e.used_bytes<e.quota_bytes)`, n.id, b.ID, userID, periodID).Scan(&issued)
				if err != nil {
					serverError(w, err)
					return
				}
				if !issued {
					continue
				}

				credential := n.uuid
				if b.Protocol == "trojan" {
					credential = n.secret
				}
				// Full IDs make names unique even across nodes with identical display names.
				displayName := strings.Map(func(r rune) rune {
					if unicode.IsControl(r) {
						return ' '
					}
					return r
				}, n.name)
				name := displayName + " · " + b.ID + " · " + n.id
				endpoints = append(endpoints, subscription.Endpoint{Name: name, Protocol: b.Protocol, Host: b.PublicHost, Port: b.PublicPort, ServerName: b.ServerName, Credential: credential})
			}
		}
	}
	data, contentType, err := subscription.Render(format, endpoints, 10808)
	if err != nil {
		serverError(w, errors.New("subscription rendering failed"))
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func validPublication(b agentwire.BindingConfig) bool {
	if b.PublicHost == "" && b.PublicPort == 0 {
		return true
	}
	// Share the encoder validation, with a generated-format placeholder credential.
	_, _, err := subscription.Render("uri", []subscription.Endpoint{{Name: "validate", Protocol: "vless", Host: b.PublicHost, Port: b.PublicPort, ServerName: b.ServerName, Credential: "00000000-0000-0000-0000-000000000001"}}, 10808)
	return err == nil && !strings.ContainsAny(b.PublicHost, "%[]")
}
