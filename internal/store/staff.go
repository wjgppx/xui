package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRoleForbidden = errors.New("role actor is no longer an administrator")
var ErrRoleConflict = errors.New("role change would remove administrative access")
var ErrRoleSessionInvalid = errors.New("role operation session is no longer valid")
var ErrRoleMFARequired = errors.New("role operation requires current second-factor verification")
var ErrRoleMFAEnrollmentRequired = errors.New("role operation requires second-factor enrollment")

type staffRoleSession struct {
	hash       string
	version    int64
	requireMFA bool
}

func ValidRole(role string) bool {
	return role == "user" || role == "admin" || role == "support" || role == "finance" || role == "operator"
}

// Maintenance is local CLI recovery only. Web callers cannot demote themselves
// or the final administrator. Role changes invalidate all existing sessions.
func (s *Store) SetUserRole(ctx context.Context, actor, target, role string, maintenance bool) error {
	return s.setUserRole(ctx, actor, target, role, maintenance, nil)
}

// SetUserRoleForSession rechecks the web actor's session after acquiring the
// role-operation lock. Queued requests cannot outlive password/MFA resets,
// logout, login expiry or step-up expiry. Host-console recovery stays separate.
func (s *Store) SetUserRoleForSession(ctx context.Context, actor, target, role, sessionHash string, version int64, requireMFA bool) error {
	if len(sessionHash) != 64 || version < 0 {
		return ErrRoleSessionInvalid
	}
	return s.setUserRole(ctx, actor, target, role, false, &staffRoleSession{hash: sessionHash, version: version, requireMFA: requireMFA})
}

func (s *Store) setUserRole(ctx context.Context, actor, target, role string, maintenance bool, session *staffRoleSession) error {
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
		var version int64
		var verified *time.Time
		if err = tx.QueryRow(ctx, `SELECT role,security_version,email_verified_at FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&actorRole, &version, &verified); err != nil {
			return err
		}
		if actorRole != "admin" {
			return ErrRoleForbidden
		}
		if session != nil {
			if version != session.version {
				return ErrRoleSessionInvalid
			}
			if s.RequireVerified && verified == nil {
				return ErrEmailUnverified
			}
			if err = authorizeRoleSession(ctx, tx, actor, session); err != nil {
				return err
			}
		}
	}
	var previous string
	if err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR NO KEY UPDATE`, target).Scan(&previous); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if previous == role {
		if session != nil {
			if err = authorizeRoleSession(ctx, tx, actor, session); err != nil {
				return err
			}
		}
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
	// A target/account/audit lock may have queued after the initial actor check.
	// Recheck natural session/step-up expiry at the actual commit boundary.
	if session != nil {
		if err = authorizeRoleSession(ctx, tx, actor, session); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func authorizeRoleSession(ctx context.Context, tx pgx.Tx, actor string, session *staffRoleSession) error {
	var expires, now time.Time
	var verifiedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT expires_at,mfa_verified_at FROM sessions WHERE token_hash=$1 AND user_id=$2 AND security_version=$3 FOR UPDATE`, session.hash, actor, session.version).Scan(&expires, &verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRoleSessionInvalid
	}
	if err != nil {
		return err
	}
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp(),EXISTS(SELECT 1 FROM staff_mfa WHERE user_id=$1 AND enabled_at IS NOT NULL)`, actor).Scan(&now, &enabled); err != nil {
		return err
	}
	if !expires.After(now) {
		return ErrRoleSessionInvalid
	}
	if session.requireMFA && !enabled {
		return ErrRoleMFAEnrollmentRequired
	}
	if enabled && (verifiedAt == nil || !verifiedAt.Add(MFASessionLifetime).After(now)) {
		return ErrRoleMFARequired
	}
	return nil
}
