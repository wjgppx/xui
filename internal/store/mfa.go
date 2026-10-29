package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/mfa"
	"golang.org/x/crypto/bcrypt"
)

var ErrMFAInvalid = errors.New("second-factor credential or account state invalid")
var ErrMFAConflict = errors.New("second-factor state changed")
var ErrMFAUnavailable = errors.New("second-factor encryption unavailable")

const MFASessionLifetime = 15 * time.Minute

func IsStaffRole(role string) bool {
	return role == "admin" || role == "finance" || role == "operator" || role == "support"
}

type MFAStatus struct {
	Enabled                bool       `json:"enabled"`
	SetupPending           bool       `json:"setup_pending"`
	VerifiedUntil          *time.Time `json:"verified_until"`
	RecoveryCodesRemaining int        `json:"recovery_codes_remaining"`
}
type MFASetup struct {
	Secret     string    `json:"secret"`
	OTPAuthURL string    `json:"otpauth_url"`
	ExpiresAt  time.Time `json:"expires_at"`
}
type MFAConfirmation struct {
	RecoveryCodes []string  `json:"recovery_codes"`
	SessionToken  string    `json:"-"`
	User          User      `json:"-"`
	VerifiedUntil time.Time `json:"-"`
}

func (s *Store) MFAStatus(ctx context.Context, userID, sessionHash string) (MFAStatus, error) {
	var status MFAStatus
	err := s.DB.QueryRow(ctx, `SELECT m.enabled_at IS NOT NULL,
 COALESCE(m.pending_expires_at>clock_timestamp() AND m.pending_version=u.security_version AND m.pending_session_hash=$2,false),
 CASE WHEN m.enabled_at IS NOT NULL AND sess.mfa_verified_at>clock_timestamp()-interval '15 minutes' THEN LEAST(sess.mfa_verified_at+interval '15 minutes',sess.expires_at) ELSE NULL END,
 (SELECT count(*) FROM staff_mfa_recovery r WHERE r.user_id=u.id AND r.used_at IS NULL)
 FROM users u LEFT JOIN staff_mfa m ON m.user_id=u.id
 LEFT JOIN sessions sess ON sess.user_id=u.id AND sess.token_hash=$2 AND sess.security_version=u.security_version AND sess.expires_at>clock_timestamp()
 WHERE u.id=$1`, userID, sessionHash).Scan(&status.Enabled, &status.SetupPending, &status.VerifiedUntil, &status.RecoveryCodesRemaining)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return status, err
}

// Lock user then session then factor, matching password/role/session invalidation.
// No factor operation can renew an already invalidated authentication generation.
func (s *Store) lockMFAUser(ctx context.Context, tx pgx.Tx, userID, sessionHash string, version int64) (User, string, error) {
	var user User
	var password string
	err := tx.QueryRow(ctx, `SELECT id,email,role,email_verified_at,security_version,password_hash FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&user.ID, &user.Email, &user.Role, &user.EmailVerifiedAt, &user.SecurityVersion, &password)
	if errors.Is(err, pgx.ErrNoRows) {
		return user, "", ErrRoleForbidden
	}
	if err != nil {
		return user, "", err
	}
	if !IsStaffRole(user.Role) || version < 0 || user.SecurityVersion != version {
		return user, "", ErrRoleForbidden
	}
	if s.RequireVerified && user.EmailVerifiedAt == nil {
		return user, "", ErrEmailUnverified
	}
	var ignored string
	err = tx.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE token_hash=$1 AND user_id=$2 AND security_version=$3 AND expires_at>clock_timestamp() FOR UPDATE`, sessionHash, userID, version).Scan(&ignored)
	if errors.Is(err, pgx.ErrNoRows) {
		return user, "", ErrRoleForbidden
	}
	return user, password, err
}
func validMFAPassword(password, hash string) bool {
	return len(password) >= 12 && len(password) <= 72 && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (s *Store) BeginMFASetup(ctx context.Context, userID, sessionHash, password string, version int64, key []byte) (MFASetup, error) {
	var setup MFASetup
	if len(key) != 32 {
		return setup, ErrMFAUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return setup, err
	}
	defer tx.Rollback(ctx)
	user, passwordHash, err := s.lockMFAUser(ctx, tx, userID, sessionHash, version)
	if err != nil {
		return setup, err
	}
	if !validMFAPassword(password, passwordHash) {
		return setup, ErrMFAInvalid
	}
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM staff_mfa WHERE user_id=$1 AND enabled_at IS NOT NULL)`, userID).Scan(&enabled); err != nil {
		return setup, err
	}
	if enabled {
		return setup, ErrMFAConflict
	}
	secret, err := mfa.GenerateSecret()
	if err != nil {
		return setup, ErrMFAUnavailable
	}
	encrypted, err := mfa.Encrypt(key, userID, secret)
	if err != nil {
		return setup, ErrMFAUnavailable
	}
	err = tx.QueryRow(ctx, `INSERT INTO staff_mfa(user_id,pending_cipher,pending_expires_at,pending_version,pending_session_hash) VALUES($1,$2,clock_timestamp()+interval '10 minutes',$3,$4)
 ON CONFLICT(user_id) DO UPDATE SET pending_cipher=EXCLUDED.pending_cipher,pending_expires_at=EXCLUDED.pending_expires_at,pending_version=EXCLUDED.pending_version,pending_session_hash=EXCLUDED.pending_session_hash
 RETURNING pending_expires_at`, userID, encrypted, version, sessionHash).Scan(&setup.ExpiresAt)
	if err != nil {
		return setup, err
	}
	if err = actionAudit(ctx, tx, userID, "account.mfa_setup_requested", userID); err != nil {
		return setup, err
	}
	setup.Secret, setup.OTPAuthURL = secret, mfa.EnrollmentURI(secret, user.Email)
	return setup, tx.Commit(ctx)
}

func (s *Store) ConfirmMFA(ctx context.Context, userID, sessionHash, code string, version int64, key []byte) (MFAConfirmation, error) {
	var result MFAConfirmation
	if len(key) != 32 {
		return result, ErrMFAUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	user, _, err := s.lockMFAUser(ctx, tx, userID, sessionHash, version)
	if err != nil {
		return result, err
	}
	var encrypted []byte
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT pending_cipher,clock_timestamp() FROM staff_mfa WHERE user_id=$1 AND enabled_at IS NULL AND pending_cipher IS NOT NULL AND pending_expires_at>clock_timestamp() AND pending_version=$2 AND pending_session_hash=$3 FOR UPDATE`, userID, version, sessionHash).Scan(&encrypted, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrMFAConflict
	}
	if err != nil {
		return result, err
	}
	secret, err := mfa.Decrypt(key, userID, encrypted)
	if err != nil {
		return result, ErrMFAUnavailable
	}
	counter, err := mfa.Match(secret, code, now, -1)
	if err != nil {
		return result, ErrMFAInvalid
	}
	codes, err := mfa.GenerateRecoveryCodes()
	if err != nil {
		return result, ErrMFAUnavailable
	}
	if _, err = tx.Exec(ctx, `UPDATE staff_mfa SET secret_cipher=pending_cipher,enabled_at=clock_timestamp(),last_counter=$2,pending_cipher=NULL,pending_expires_at=NULL,pending_version=NULL,pending_session_hash=NULL WHERE user_id=$1`, userID, counter); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM staff_mfa_recovery WHERE user_id=$1`, userID); err != nil {
		return result, err
	}
	for _, recovery := range codes {
		if _, err = tx.Exec(ctx, `INSERT INTO staff_mfa_recovery(user_id,code_hash) VALUES($1,$2)`, userID, mfa.RecoveryHash(userID, recovery)); err != nil {
			return result, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, userID); err != nil {
		return result, err
	}
	user.SecurityVersion++
	if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return result, err
	}
	result.SessionToken = ID() + ID()
	if err = tx.QueryRow(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,security_version,mfa_verified_at) VALUES($1,$2,clock_timestamp()+interval '24 hours',$3,clock_timestamp()) RETURNING mfa_verified_at+interval '15 minutes'`, securityHash(result.SessionToken), userID, user.SecurityVersion).Scan(&result.VerifiedUntil); err != nil {
		return result, err
	}
	if err = actionAudit(ctx, tx, userID, "account.mfa_enabled", userID); err != nil {
		return result, err
	}
	result.RecoveryCodes, result.User = codes, user
	return result, tx.Commit(ctx)
}

func consumeMFACredential(ctx context.Context, tx pgx.Tx, userID, code string, key []byte) (time.Time, error) {
	var encrypted []byte
	var counter int64
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT secret_cipher,last_counter,clock_timestamp() FROM staff_mfa WHERE user_id=$1 AND enabled_at IS NOT NULL FOR UPDATE`, userID).Scan(&encrypted, &counter, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return now, ErrMFAConflict
	}
	if err != nil {
		return now, err
	}
	secret, err := mfa.Decrypt(key, userID, encrypted)
	if err != nil {
		return now, ErrMFAUnavailable
	}
	if mfa.TOTPCode(code) {
		next, err := mfa.Match(secret, code, now, counter)
		if err != nil {
			return now, ErrMFAInvalid
		}
		_, err = tx.Exec(ctx, `UPDATE staff_mfa SET last_counter=$2 WHERE user_id=$1`, userID, next)
		return now, err
	}
	if !mfa.RecoveryCode(code) {
		return now, ErrMFAInvalid
	}
	tag, err := tx.Exec(ctx, `UPDATE staff_mfa_recovery SET used_at=clock_timestamp() WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL`, userID, mfa.RecoveryHash(userID, code))
	if err != nil {
		return now, err
	}
	if tag.RowsAffected() != 1 {
		return now, ErrMFAInvalid
	}
	return now, nil
}
func (s *Store) VerifyMFA(ctx context.Context, userID, sessionHash, code string, version int64, key []byte) (time.Time, error) {
	var until time.Time
	if len(key) != 32 {
		return until, ErrMFAUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return until, err
	}
	defer tx.Rollback(ctx)
	if _, _, err = s.lockMFAUser(ctx, tx, userID, sessionHash, version); err != nil {
		return until, err
	}
	if _, err = consumeMFACredential(ctx, tx, userID, code, key); err != nil {
		return until, err
	}
	if err = tx.QueryRow(ctx, `UPDATE sessions SET mfa_verified_at=clock_timestamp() WHERE token_hash=$1 AND user_id=$2 RETURNING LEAST(mfa_verified_at+interval '15 minutes',expires_at)`, sessionHash, userID).Scan(&until); err != nil {
		return until, err
	}
	if err = actionAudit(ctx, tx, userID, "account.mfa_verified", userID); err != nil {
		return until, err
	}
	return until, tx.Commit(ctx)
}
func (s *Store) DisableMFA(ctx context.Context, userID, sessionHash, password, code string, version int64, key []byte) error {
	if len(key) != 32 {
		return ErrMFAUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, passwordHash, err := s.lockMFAUser(ctx, tx, userID, sessionHash, version)
	if err != nil {
		return err
	}
	if !validMFAPassword(password, passwordHash) {
		return ErrMFAInvalid
	}
	if _, err = consumeMFACredential(ctx, tx, userID, code, key); err != nil {
		return err
	}
	if err = clearStaffMFA(ctx, tx, userID); err != nil {
		return err
	}
	if err = actionAudit(ctx, tx, userID, "account.mfa_disabled", userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func clearStaffMFA(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM staff_mfa_recovery WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM staff_mfa WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	return nil
}

// ResetStaffMFA is a host-console break-glass operation, never an HTTP recovery
// path. The caller must authenticate the privileged host operator before use.
func (s *Store) ResetStaffMFA(ctx context.Context, actor, userID string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var role string
	if err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&role); err != nil {
		return err
	}
	if !IsStaffRole(role) {
		return ErrRoleForbidden
	}
	if err = clearStaffMFA(ctx, tx, userID); err != nil {
		return err
	}
	if err = actionAudit(ctx, tx, actor, "account.mfa_host_reset", userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
