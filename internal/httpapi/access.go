package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/store"
)

type accessGroup struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Revision int64    `json:"revision"`
	NodeIDs  []string `json:"node_ids"`
}
type accessNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type accessPlan struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Revision int64    `json:"revision"`
	GroupIDs []string `json:"group_ids"`
}

func (s *Server) accessOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	groups := []accessGroup{}
	nodes := []accessNode{}
	plans := []accessPlan{}
	rows, err := s.Store.DB.Query(r.Context(), `SELECT g.id,g.name,g.enabled,g.revision,COALESCE(array_agg(m.node_id ORDER BY m.node_id) FILTER(WHERE m.node_id IS NOT NULL),'{}'::text[]) FROM node_groups g LEFT JOIN node_group_members m ON m.group_id=g.id GROUP BY g.id ORDER BY g.created_at,g.id`)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var g accessGroup
		if err = rows.Scan(&g.ID, &g.Name, &g.Enabled, &g.Revision, &g.NodeIDs); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		groups = append(groups, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		serverError(w, err)
		return
	}
	rows, err = s.Store.DB.Query(r.Context(), `SELECT id,name FROM nodes ORDER BY created_at,id`)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var n accessNode
		if err = rows.Scan(&n.ID, &n.Name); err != nil {
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
	rows, err = s.Store.DB.Query(r.Context(), `SELECT p.id,p.name,p.access_revision,COALESCE(array_agg(g.group_id ORDER BY g.group_id) FILTER(WHERE g.group_id IS NOT NULL),'{}'::text[]) FROM plans p LEFT JOIN plan_node_groups g ON g.plan_id=p.id GROUP BY p.id ORDER BY p.created_at,p.id`)
	if err != nil {
		serverError(w, err)
		return
	}
	for rows.Next() {
		var p accessPlan
		if err = rows.Scan(&p.ID, &p.Name, &p.Revision, &p.GroupIDs); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		plans = append(plans, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]any{"groups": groups, "nodes": nodes, "plans": plans})
}

type groupMutation struct {
	Name     string   `json:"name"`
	NodeIDs  []string `json:"node_ids"`
	Enabled  *bool    `json:"enabled"`
	Revision int64    `json:"revision"`
}

func validAccessIDs(ids []string) bool {
	if ids == nil || len(ids) > 100 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if len(id) != 32 || seen[id] {
			return false
		}
		if _, err := hex.DecodeString(id); err != nil {
			return false
		}
		seen[id] = true
	}
	return true
}
func validGroupName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s *Server) createNodeGroup(w http.ResponseWriter, r *http.Request) {
	s.mutateNodeGroup(w, r, false)
}
func (s *Server) updateNodeGroup(w http.ResponseWriter, r *http.Request) {
	s.mutateNodeGroup(w, r, true)
}
func (s *Server) mutateNodeGroup(w http.ResponseWriter, r *http.Request, update bool) {
	actor, ok := s.current(w, r, true)
	if !ok {
		return
	}
	var body groupMutation
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if !validGroupName(body.Name) || !validAccessIDs(body.NodeIDs) || body.Enabled == nil || (update && body.Revision < 1) || (!update && body.Revision != 0) {
		fail(w, 400, "需要名称、启用状态、唯一节点列表和有效版本")
		return
	}
	sort.Strings(body.NodeIDs)
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	id := store.ID()
	revision := int64(1)
	oldNodes := []string{}
	action := "node_group.created"
	if update {
		id = r.PathValue("id")
		action = "node_group.updated"
		err = tx.QueryRow(r.Context(), `SELECT revision FROM node_groups WHERE id=$1 FOR UPDATE`, id).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 404, "节点组不存在")
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		if revision != body.Revision {
			fail(w, 409, "节点组已更新，请重新读取")
			return
		}
		revision++
		rows, err := tx.Query(r.Context(), `SELECT node_id FROM node_group_members WHERE group_id=$1 ORDER BY node_id`, id)
		if err != nil {
			serverError(w, err)
			return
		}
		for rows.Next() {
			var nodeID string
			if err = rows.Scan(&nodeID); err != nil {
				rows.Close()
				serverError(w, err)
				return
			}
			oldNodes = append(oldNodes, nodeID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			serverError(w, err)
			return
		}
	}
	var found int
	err = tx.QueryRow(r.Context(), `SELECT count(*) FROM nodes WHERE id=ANY($1::text[])`, body.NodeIDs).Scan(&found)
	if err != nil {
		serverError(w, err)
		return
	}
	if found != len(body.NodeIDs) {
		fail(w, 400, "节点列表含不存在的节点")
		return
	}
	if update {
		_, err = tx.Exec(r.Context(), `UPDATE node_groups SET name=$1,enabled=$2,revision=$3 WHERE id=$4`, body.Name, *body.Enabled, revision, id)
	} else {
		_, err = tx.Exec(r.Context(), `INSERT INTO node_groups(id,name,enabled) VALUES($1,$2,$3)`, id, body.Name, *body.Enabled)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM node_group_members WHERE group_id=$1`, id)
	}
	if err != nil {
		serverError(w, err)
		return
	}
	for _, nodeID := range body.NodeIDs {
		if _, err = tx.Exec(r.Context(), `INSERT INTO node_group_members(group_id,node_id) VALUES($1,$2)`, id, nodeID); err != nil {
			serverError(w, err)
			return
		}
	}
	targets := map[string]bool{}
	for _, nodeID := range oldNodes {
		targets[nodeID] = true
	}
	for _, nodeID := range body.NodeIDs {
		targets[nodeID] = true
	}
	affected := make([]string, 0, len(targets))
	for nodeID := range targets {
		affected = append(affected, nodeID)
	}
	sort.Strings(affected)
	payload, _ := json.Marshal(map[string]any{"node_ids": affected})
	aggregate := id + ":" + integerRevision(revision)
	_, err = tx.Exec(r.Context(), `INSERT INTO outbox_events(id,kind,aggregate_id,payload) VALUES($1,'node_group.changed',$2,$3)`, store.ID(), aggregate, payload)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,$2,$3)`, actor.ID, action, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	status := 201
	if update {
		status = 200
	}
	reply(w, status, accessGroup{ID: id, Name: body.Name, Enabled: *body.Enabled, Revision: revision, NodeIDs: body.NodeIDs})
}
func integerRevision(value int64) string { return strconv.FormatInt(value, 10) }

func (s *Server) setPlanNodeGroups(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.current(w, r, true)
	if !ok {
		return
	}
	var body struct {
		GroupIDs []string `json:"group_ids"`
		Revision int64    `json:"revision"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validAccessIDs(body.GroupIDs) || body.Revision < 1 {
		fail(w, 400, "需要唯一节点组列表和当前版本")
		return
	}
	sort.Strings(body.GroupIDs)
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var revision int64
	err = tx.QueryRow(r.Context(), `SELECT access_revision FROM plans WHERE id=$1 FOR UPDATE`, r.PathValue("id")).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "套餐不存在")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if revision != body.Revision {
		fail(w, 409, "套餐授权已更新，请重新读取")
		return
	}
	var found int
	err = tx.QueryRow(r.Context(), `SELECT count(*) FROM node_groups WHERE id=ANY($1::text[])`, body.GroupIDs).Scan(&found)
	if err != nil {
		serverError(w, err)
		return
	}
	if found != len(body.GroupIDs) {
		fail(w, 400, "节点组列表含不存在的节点组")
		return
	}
	_, err = tx.Exec(r.Context(), `DELETE FROM plan_node_groups WHERE plan_id=$1`, r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	for _, id := range body.GroupIDs {
		if _, err = tx.Exec(r.Context(), `INSERT INTO plan_node_groups(plan_id,group_id) VALUES($1,$2)`, r.PathValue("id"), id); err != nil {
			serverError(w, err)
			return
		}
	}
	revision++
	_, err = tx.Exec(r.Context(), `UPDATE plans SET access_revision=$1 WHERE id=$2`, revision, r.PathValue("id"))
	payload, _ := json.Marshal(map[string]string{"plan_id": r.PathValue("id")})
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO outbox_events(id,kind,aggregate_id,payload) VALUES($1,'plan.access_changed',$2,$3)`, store.ID(), r.PathValue("id")+":"+integerRevision(revision), payload)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'plan.node_groups_updated',$2)`, actor.ID, r.PathValue("id"))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]any{"ok": true, "revision": revision})
}

type businessEvent struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	AggregateID string     `json:"aggregate_id"`
	Attempts    int        `json:"attempt_count"`
	LastError   string     `json:"last_error"`
	AvailableAt time.Time  `json:"available_at"`
	ProcessedAt *time.Time `json:"processed_at"`
	DeadAt      *time.Time `json:"dead_lettered_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	rows, err := s.Store.DB.Query(r.Context(), `SELECT id,kind,aggregate_id,attempts,last_error,available_at,processed_at,dead_lettered_at,created_at FROM outbox_events ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	events := []businessEvent{}
	for rows.Next() {
		var e businessEvent
		if err = rows.Scan(&e.ID, &e.Kind, &e.AggregateID, &e.Attempts, &e.LastError, &e.AvailableAt, &e.ProcessedAt, &e.DeadAt, &e.CreatedAt); err != nil {
			serverError(w, err)
			return
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, events)
}
func (s *Server) retryEvent(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.current(w, r, true)
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
	var processed, dead *time.Time
	var attempts int
	err = tx.QueryRow(r.Context(), `SELECT processed_at,dead_lettered_at,attempts FROM outbox_events WHERE id=$1 FOR UPDATE`, r.PathValue("id")).Scan(&processed, &dead, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "事件不存在")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if processed != nil || (dead == nil && attempts == 0) {
		fail(w, 409, "仅失败的未完成事件可以重试")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE outbox_events SET attempts=0,last_error='',available_at=now(),dead_lettered_at=NULL WHERE id=$1`, r.PathValue("id"))
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'event.retry_requested',$2)`, actor.ID, r.PathValue("id"))
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
func (s *Server) revokeEntitlement(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.current(w, r, true)
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
	var userID string
	var revoked *time.Time
	err = tx.QueryRow(r.Context(), `SELECT user_id,revoked_at FROM entitlement_periods WHERE id=$1 FOR NO KEY UPDATE`, r.PathValue("id")).Scan(&userID, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "权益不存在")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if revoked != nil {
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE entitlement_periods SET revoked_at=now() WHERE id=$1`, r.PathValue("id"))
	payload, _ := json.Marshal(map[string]string{"user_id": userID, "entitlement_id": r.PathValue("id")})
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO outbox_events(id,kind,aggregate_id,payload) VALUES($1,'entitlement.revoked',$2,$3) ON CONFLICT(kind,aggregate_id) DO NOTHING`, store.ID(), r.PathValue("id"), payload)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'entitlement.revoked',$2)`, actor.ID, r.PathValue("id"))
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
