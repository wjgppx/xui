package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrBindingCapacity = errors.New("binding authorization exceeds 100 users")

type AuthorizedSubject struct {
	UserID        string
	EntitlementID string
	ExpiresAt     time.Time
}

// BindingSubjects evaluates current entitlement state directly. An event is an
// invalidation hint, never the authority for granting access. Existing manual
// bindings retain their allowlist semantics; plan bindings require a purchased
// group snapshot and current membership in an enabled operational node group.
func (s *Store) BindingSubjects(ctx context.Context, tx pgx.Tx, nodeID, mode string, emails []string, now time.Time) ([]AuthorizedSubject, error) {
	if mode == "" {
		mode = "manual"
	}
	if mode != "manual" && mode != "plan" {
		return nil, errors.New("unsupported binding access mode")
	}
	rows, err := tx.Query(ctx, `SELECT u.id,e.id,e.ends_at
 FROM users u JOIN LATERAL (
  SELECT ep.id,ep.ends_at FROM entitlement_periods ep
  WHERE ep.user_id=u.id AND ep.starts_at<=$4 AND ep.ends_at>$4
   AND ep.used_bytes<ep.quota_bytes AND ep.revoked_at IS NULL
   AND (($2='manual' AND u.email=ANY($3::text[])) OR ($2='plan' AND EXISTS(
    SELECT 1 FROM entitlement_node_groups eg
    JOIN node_group_members gm ON gm.group_id=eg.group_id AND gm.node_id=$1
    JOIN node_groups ng ON ng.id=gm.group_id AND ng.enabled
    WHERE eg.entitlement_period_id=ep.id
   )))
  ORDER BY ep.starts_at DESC LIMIT 1
 ) e ON true WHERE (NOT $5 OR u.email_verified_at IS NOT NULL) ORDER BY u.id LIMIT 101`, nodeID, mode, emails, now, s.RequireVerified)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	subjects := []AuthorizedSubject{}
	for rows.Next() {
		var subject AuthorizedSubject
		if err = rows.Scan(&subject.UserID, &subject.EntitlementID, &subject.ExpiresAt); err != nil {
			return nil, err
		}
		subjects = append(subjects, subject)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(subjects) > 100 {
		return nil, ErrBindingCapacity
	}
	return subjects, nil
}

func (s *Store) EnsureNodeGrant(ctx context.Context, tx pgx.Tx, nodeID, userID string) (string, string, error) {
	raw := ID()
	uuid := fmt.Sprintf("%s-%s-%s-%s-%s", raw[:8], raw[8:12], raw[12:16], raw[16:20], raw[20:])
	secret := ID() + ID()
	// The surrounding node lock serializes this with credential rotation. A no-op
	// conflict update returns the persistent credential, never the proposed one.
	err := tx.QueryRow(ctx, `INSERT INTO node_grants(node_id,user_id,credential_id,secret) VALUES($1,$2,$3,$4)
 ON CONFLICT(node_id,user_id) DO UPDATE SET credential_id=node_grants.credential_id
 RETURNING credential_id,secret`, nodeID, userID, uuid, secret).Scan(&uuid, &secret)
	return uuid, secret, err
}
