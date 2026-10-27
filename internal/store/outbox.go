package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var eventIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ScheduleEntitlementEvents makes time transitions durable. Duplicate scans and
// concurrent controls converge on the existing (kind, aggregate_id) constraint.
func (s *Store) ScheduleEntitlementEvents(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO outbox_events(id,kind,aggregate_id,payload)
 SELECT md5('activation:'||id),'entitlement.activated',id,jsonb_build_object('user_id',user_id,'entitlement_id',id)
 FROM entitlement_periods e WHERE starts_at<=now() AND ends_at>now() AND revoked_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM outbox_events ev WHERE ev.kind='entitlement.activated' AND ev.aggregate_id=e.id)
 ON CONFLICT(kind,aggregate_id) DO NOTHING`)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO outbox_events(id,kind,aggregate_id,payload)
 SELECT md5('expiration:'||id),'entitlement.expired',id,jsonb_build_object('user_id',user_id,'entitlement_id',id)
 FROM entitlement_periods e WHERE ends_at<=now()
 AND NOT EXISTS(SELECT 1 FROM outbox_events ev WHERE ev.kind='entitlement.expired' AND ev.aggregate_id=e.id)
 ON CONFLICT(kind,aggregate_id) DO NOTHING`)
	return err
}

type pendingEvent struct {
	id, kind string
	payload  []byte
}

var errNoEvent = errors.New("no event available")

type eventFailure struct{ code string }

func (e *eventFailure) Error() string { return e.code }
func deliveryError() error            { return &eventFailure{code: "delivery_failed"} }

func eventNodeIDs(ctx context.Context, tx pgx.Tx, event pendingEvent) ([]string, error) {
	var payload struct {
		UserID        string   `json:"user_id"`
		EntitlementID string   `json:"entitlement_id"`
		NodeIDs       []string `json:"node_ids"`
		PlanID        string   `json:"plan_id"`
	}
	if err := json.Unmarshal(event.payload, &payload); err != nil {
		return nil, &eventFailure{code: "invalid_payload"}
	}
	switch event.kind {
	case "entitlement.granted", "entitlement.revoked", "entitlement.activated", "entitlement.expired":
		if !eventIDPattern.MatchString(payload.UserID) || !eventIDPattern.MatchString(payload.EntitlementID) {
			return nil, &eventFailure{code: "invalid_payload"}
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM entitlement_periods WHERE id=$1 AND user_id=$2)`, payload.EntitlementID, payload.UserID).Scan(&exists); err != nil {
			return nil, deliveryError()
		}
		if !exists {
			return nil, &eventFailure{code: "invalid_payload"}
		}
		rows, err := tx.Query(ctx, `SELECT id FROM nodes WHERE revoked_at IS NULL AND (id IN (SELECT node_id FROM node_grants WHERE user_id=$1) OR id IN(SELECT gm.node_id FROM node_group_members gm JOIN entitlement_node_groups eg ON eg.group_id=gm.group_id WHERE eg.entitlement_period_id=$2)) ORDER BY id FOR UPDATE`, payload.UserID, payload.EntitlementID)
		if err != nil {
			return nil, deliveryError()
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return nil, deliveryError()
			}
			ids = append(ids, id)
		}
		if rows.Err() != nil {
			return nil, deliveryError()
		}
		return ids, nil
	case "node_group.changed":
		if payload.NodeIDs == nil || len(payload.NodeIDs) > 1000 {
			return nil, &eventFailure{code: "invalid_payload"}
		}
		for _, id := range payload.NodeIDs {
			if !eventIDPattern.MatchString(id) {
				return nil, &eventFailure{code: "invalid_payload"}
			}
		}
		rows, err := tx.Query(ctx, `SELECT id FROM nodes WHERE id=ANY($1::text[]) AND revoked_at IS NULL ORDER BY id FOR UPDATE`, payload.NodeIDs)
		if err != nil {
			return nil, deliveryError()
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return nil, deliveryError()
			}
			ids = append(ids, id)
		}
		if rows.Err() != nil {
			return nil, deliveryError()
		}
		return ids, nil
	case "plan.access_changed":
		if payload.PlanID == "" || len(payload.PlanID) > 100 {
			return nil, &eventFailure{code: "invalid_payload"}
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plans WHERE id=$1)`, payload.PlanID).Scan(&exists); err != nil {
			return nil, deliveryError()
		}
		if !exists {
			return nil, &eventFailure{code: "invalid_payload"}
		}
		return []string{}, nil
	default:
		return nil, &eventFailure{code: "unsupported_kind"}
	}
}

func (s *Store) processOneEvent(ctx context.Context) (string, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var event pendingEvent
	err = tx.QueryRow(ctx, `SELECT id,kind,payload FROM outbox_events WHERE processed_at IS NULL AND dead_lettered_at IS NULL AND available_at<=now() ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&event.id, &event.kind, &event.payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNoEvent
	}
	if err != nil {
		return "", err
	}
	nodes, err := eventNodeIDs(ctx, tx, event)
	if err != nil {
		return event.id, err
	}
	for _, nodeID := range nodes {
		var inserted bool
		err = tx.QueryRow(ctx, `WITH delivered AS (INSERT INTO outbox_deliveries(event_id,node_id) VALUES($1,$2) ON CONFLICT DO NOTHING RETURNING 1) SELECT EXISTS(SELECT 1 FROM delivered)`, event.id, nodeID).Scan(&inserted)
		if err != nil {
			return event.id, deliveryError()
		}
		if inserted {
			if _, err = tx.Exec(ctx, `UPDATE nodes SET desired_revision=desired_revision+1,desired_digest='',apply_status='pending',apply_error='' WHERE id=$1`, nodeID); err != nil {
				return event.id, deliveryError()
			}
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox_events SET processed_at=now(),last_error='' WHERE id=$1`, event.id); err != nil {
		return event.id, deliveryError()
	}
	if err = tx.Commit(ctx); err != nil {
		return event.id, deliveryError()
	}
	return event.id, nil
}

func (s *Store) recordEventFailure(ctx context.Context, id string, err error) error {
	code := "delivery_failed"
	var failure *eventFailure
	if errors.As(err, &failure) {
		code = failure.code
	}
	// The failed effects transaction has already rolled back. A concurrent worker
	// can finish the event before this update; in that case no retry state changes.
	_, saveErr := s.DB.Exec(ctx, `UPDATE outbox_events SET attempts=attempts+1,last_error=$2,
 available_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(attempts+1,8))::integer)),
 dead_lettered_at=CASE WHEN attempts+1>=5 THEN now() ELSE NULL END
 WHERE id=$1 AND processed_at IS NULL AND dead_lettered_at IS NULL`, id, code)
	return saveErr
}

// ProcessOutbox handles at most limit events. Successful node invalidations and
// delivery/processed markers share one transaction, making uncertain commits and
// replay harmless. A poison event is backoff scheduled without blocking peers.
func (s *Store) ProcessOutbox(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("outbox limit must be 1..1000")
	}
	completed := 0
	for i := 0; i < limit; i++ {
		id, err := s.processOneEvent(ctx)
		if errors.Is(err, errNoEvent) {
			return completed, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return completed, ctx.Err()
			}
			if id == "" {
				return completed, err
			}
			if err = s.recordEventFailure(ctx, id, err); err != nil {
				return completed, err
			}
			continue
		}
		completed++
	}
	return completed, nil
}

func (s *Store) RunOutbox(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.ScheduleEntitlementEvents(ctx); err != nil {
			if ctx.Err() == nil {
				slog.Warn("outbox transition scan failed")
			}
		}
		if _, err := s.ProcessOutbox(ctx, 50); err != nil {
			if ctx.Err() == nil {
				slog.Warn("outbox delivery failed")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
