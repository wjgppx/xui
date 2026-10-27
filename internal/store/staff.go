package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrRoleForbidden = errors.New("role actor is no longer an administrator")
var ErrRoleConflict = errors.New("role change would remove administrative access")

func ValidRole(role string) bool {
	return role == "user" || role == "admin" || role == "support" || role == "finance" || role == "operator"
}

// Maintenance is local CLI recovery only. Web callers cannot demote themselves
// or the final administrator. Role changes invalidate all existing sessions.
func (s *Store) SetUserRole(ctx context.Context, actor, target, role string, maintenance bool) error {
	if !ValidRole(role) {
		return ErrRoleConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(738912409)`); err != nil {
		return err
	}
	if !maintenance {
		var actorRole string
		if err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&actorRole); err != nil {
			return err
		}
		if actorRole != "admin" {
			return ErrRoleForbidden
		}
	}
	var previous string
	if err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR NO KEY UPDATE`, target).Scan(&previous); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if previous == role {
		return tx.Commit(ctx)
	}
	if !maintenance && role != "admin" && previous == "admin" {
		var admins int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
			return err
		}
		if actor == target || admins <= 1 {
			return ErrRoleConflict
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET role=$1,security_version=security_version+1 WHERE id=$2`, role, target); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, target); err != nil {
		return err
	}
	action := "staff.role_changed"
	if maintenance {
		action = "staff.role_changed_via_cli"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,$2,$3)`, actor, action, target); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
