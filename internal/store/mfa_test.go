package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/mfa"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

const mfaPassword = "safe-mfa-fixture-password"

func mfaFixture(t *testing.T) (*store.Store, string, string, []byte) {
	t.Helper()
	s := testutil.Database(t)
	id := testutil.User(t, s)
	hash, err := bcrypt.GenerateFromPassword([]byte(mfaPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(context.Background(), `UPDATE users SET role='admin',password_hash=$2,email_verified_at=clock_timestamp() WHERE id=$1`, id, string(hash)); err != nil {
		t.Fatal(err)
	}
	token := mfaSession(t, s, id, 0)
	return s, id, token, bytes.Repeat([]byte{8}, 32)
}
func mfaSession(t *testing.T, s *store.Store, id string, version int64) string {
	t.Helper()
	token := store.ID() + store.ID()
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO sessions(token_hash,user_id,security_version,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '24 hours')`, hashSecurity(token), id, version); err != nil {
		t.Fatal(err)
	}
	return token
}
func mfaEnroll(t *testing.T, s *store.Store, id, token string, key []byte) (store.MFASetup, store.MFAConfirmation) {
	t.Helper()
	ctx := context.Background()
	setup, err := s.BeginMFASetup(ctx, id, hashSecurity(token), mfaPassword, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	code, err := mfa.Code(setup.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.ConfirmMFA(ctx, id, hashSecurity(token), code, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	return setup, confirmed
}
func TestMFAEnrollmentEncryptsSecretsRotatesSessionsAndExposesOnlySafeStatus(t *testing.T) {
	s, id, old, key := mfaFixture(t)
	ctx := context.Background()
	setup, confirmed := mfaEnroll(t, s, id, old, key)
	if confirmed.User.SecurityVersion != 1 || len(confirmed.RecoveryCodes) != 10 || len(confirmed.SessionToken) != 64 || confirmed.SessionToken == old {
		t.Fatal("enrollment result incomplete")
	}
	var stored []byte
	var version, sessions int
	if err := s.DB.QueryRow(ctx, `SELECT secret_cipher,(SELECT security_version FROM users WHERE id=$1),(SELECT count(*) FROM sessions WHERE user_id=$1) FROM staff_mfa WHERE user_id=$1`, id).Scan(&stored, &version, &sessions); err != nil || version != 1 || sessions != 1 || bytes.Contains(stored, []byte(setup.Secret)) {
		t.Fatal("secret/session enrollment state", err)
	}
	status, err := s.MFAStatus(ctx, id, hashSecurity(confirmed.SessionToken))
	if err != nil || !status.Enabled || status.SetupPending || status.VerifiedUntil == nil || status.RecoveryCodesRemaining != 10 {
		t.Fatal(status, err)
	}
	oldStatus, err := s.MFAStatus(ctx, id, hashSecurity(old))
	if err != nil || oldStatus.VerifiedUntil != nil {
		t.Fatal("old session kept step-up", err)
	}
	data, _ := json.Marshal(status)
	for _, secret := range []string{setup.Secret, confirmed.RecoveryCodes[0], confirmed.SessionToken, "cipher"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("status leaked credential")
		}
	}
	data, _ = json.Marshal(confirmed)
	if bytes.Contains(data, []byte(confirmed.SessionToken)) {
		t.Fatal("confirmation marshaled a session token")
	}
	var digest string
	if err = s.DB.QueryRow(ctx, `SELECT code_hash FROM staff_mfa_recovery WHERE user_id=$1 LIMIT 1`, id).Scan(&digest); err != nil || len(digest) != 64 {
		t.Fatal(err)
	}
	for _, code := range confirmed.RecoveryCodes {
		if code == digest {
			t.Fatal("recovery credential not hashed")
		}
	}
}
func TestMFAEnrollmentBindsSessionExpiryAndSecurityGeneration(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	setup, err := s.BeginMFASetup(ctx, id, hashSecurity(token), mfaPassword, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	other := mfaSession(t, s, id, 0)
	code, _ := mfa.Code(setup.Secret, time.Now())
	if _, err = s.ConfirmMFA(ctx, id, hashSecurity(other), code, 0, key); !errors.Is(err, store.ErrMFAConflict) {
		t.Fatal("other session confirmed pending factor", err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE staff_mfa SET pending_expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmMFA(ctx, id, hashSecurity(token), code, 0, key); !errors.Is(err, store.ErrMFAConflict) {
		t.Fatal("expired enrollment accepted", err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginMFASetup(ctx, id, hashSecurity(token), mfaPassword, 0, key); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("stale login created factor", err)
	}
}
func TestMFATOTPReplayAndConcurrentRecoveryConsumptionAreRejected(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	setup, confirmed := mfaEnroll(t, s, id, token, key)
	var counter int64
	if err := s.DB.QueryRow(ctx, `SELECT last_counter FROM staff_mfa WHERE user_id=$1`, id).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	code, _ := mfa.Code(setup.Secret, time.Unix(counter*30, 0))
	if _, err := s.VerifyMFA(ctx, id, hashSecurity(confirmed.SessionToken), code, 1, key); !errors.Is(err, store.ErrMFAInvalid) {
		t.Fatal("consumed TOTP replayed", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.VerifyMFA(ctx, id, hashSecurity(confirmed.SessionToken), confirmed.RecoveryCodes[0], 1, key)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, store.ErrMFAInvalid) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("recovery was consumed more than once", success)
	}
	status, err := s.MFAStatus(ctx, id, hashSecurity(confirmed.SessionToken))
	if err != nil || status.RecoveryCodesRemaining != 9 || status.VerifiedUntil == nil {
		t.Fatal(status, err)
	}
}
func TestMFAConcurrentTOTPAcceptsExactlyOneSession(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	setup, confirmed := mfaEnroll(t, s, id, token, key)
	// Exercise an unused current step without waiting for wall-clock rollover.
	if _, err := s.DB.Exec(ctx, `UPDATE staff_mfa SET last_counter=-1 WHERE user_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	code, err := mfa.Code(setup.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other := mfaSession(t, s, id, 1)
	tokens := []string{confirmed.SessionToken, other}
	results := make(chan error, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(session string) {
			defer wg.Done()
			_, err := s.VerifyMFA(ctx, id, hashSecurity(session), code, 1, key)
			results <- err
		}(tokens[i%len(tokens)])
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, store.ErrMFAInvalid) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("same TOTP authorized multiple sessions", success)
	}
}
func TestMFARoleChangeCannotRevivePendingOrVerifiedAuthority(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	setup, err := s.BeginMFASetup(ctx, id, hashSecurity(token), mfaPassword, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetUserRole(ctx, id, id, "user", true); err != nil {
		t.Fatal(err)
	}
	code, _ := mfa.Code(setup.Secret, time.Now())
	if _, err = s.ConfirmMFA(ctx, id, hashSecurity(token), code, 0, key); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("demoted account completed pending MFA", err)
	}
	if err = s.SetUserRole(ctx, id, id, "support", true); err != nil {
		t.Fatal(err)
	}
	fresh := mfaSession(t, s, id, 2)
	if _, err = s.ConfirmMFA(ctx, id, hashSecurity(fresh), code, 2, key); !errors.Is(err, store.ErrMFAConflict) {
		t.Fatal("new staff generation reused old enrollment", err)
	}
	status, err := s.MFAStatus(ctx, id, hashSecurity(fresh))
	if err != nil || status.SetupPending || status.VerifiedUntil != nil || status.Enabled {
		t.Fatal("role change retained old authority", status, err)
	}
}
func TestMFADisableNeedsBothProofsAndMissingOrWrongKeyFailsClosed(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	_, confirmed := mfaEnroll(t, s, id, token, key)
	session := hashSecurity(confirmed.SessionToken)
	if _, err := s.BeginMFASetup(ctx, id, session, mfaPassword, 1, key); !errors.Is(err, store.ErrMFAConflict) {
		t.Fatal("password replaced active factor", err)
	}
	for _, badKey := range [][]byte{nil, bytes.Repeat([]byte{9}, 32)} {
		if _, err := s.VerifyMFA(ctx, id, session, confirmed.RecoveryCodes[0], 1, badKey); !errors.Is(err, store.ErrMFAUnavailable) {
			t.Fatal("missing/wrong key bypassed", err)
		}
	}
	if err := s.DisableMFA(ctx, id, session, "wrong-current-password", confirmed.RecoveryCodes[0], 1, key); !errors.Is(err, store.ErrMFAInvalid) {
		t.Fatal("bad password removed MFA", err)
	}
	if err := s.DisableMFA(ctx, id, session, mfaPassword, confirmed.RecoveryCodes[0], 1, key); err != nil {
		t.Fatal(err)
	}
	status, err := s.MFAStatus(ctx, id, session)
	if err != nil || status.Enabled || status.SetupPending || status.RecoveryCodesRemaining != 0 || status.VerifiedUntil != nil {
		t.Fatal(status, err)
	}
	var version, sessions int
	if err = s.DB.QueryRow(ctx, `SELECT security_version,(SELECT count(*) FROM sessions WHERE user_id=$1) FROM users WHERE id=$1`, id).Scan(&version, &sessions); err != nil || version != 2 || sessions != 0 {
		t.Fatal("disable did not revoke sessions", err)
	}
}
func TestMFAPasswordResetPreservesFactorAndRequiresFreshSession(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	_, confirmed := mfaEnroll(t, s, id, token, key)
	reset := accountToken(t, s, id, "reset")
	newHash, _ := bcrypt.GenerateFromPassword([]byte("replacement-safe-password"), bcrypt.MinCost)
	if err := s.ResetPassword(ctx, reset, string(newHash)); err != nil {
		t.Fatal(err)
	}
	status, err := s.MFAStatus(ctx, id, hashSecurity(confirmed.SessionToken))
	if err != nil || !status.Enabled || status.VerifiedUntil != nil {
		t.Fatal("password reset bypassed MFA", status, err)
	}
	if _, err = s.VerifyMFA(ctx, id, hashSecurity(confirmed.SessionToken), confirmed.RecoveryCodes[0], 1, key); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("stale session recovered authority", err)
	}
	fresh := mfaSession(t, s, id, 2)
	if _, err = s.VerifyMFA(ctx, id, hashSecurity(fresh), confirmed.RecoveryCodes[0], 2, key); err != nil {
		t.Fatal("new login could not verify retained factor", err)
	}
}
func TestMFAEnrollmentAuditFailureRollsBackAllSecurityState(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	setup, err := s.BeginMFASetup(ctx, id, hashSecurity(token), mfaPassword, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `CREATE FUNCTION reject_mfa_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.mfa_enabled' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_mfa_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_mfa_audit()`); err != nil {
		t.Fatal(err)
	}
	code, _ := mfa.Code(setup.Secret, time.Now())
	if _, err = s.ConfirmMFA(ctx, id, hashSecurity(token), code, 0, key); err == nil {
		t.Fatal("failed audit committed factor")
	}
	status, err := s.MFAStatus(ctx, id, hashSecurity(token))
	if err != nil || status.Enabled || !status.SetupPending || status.RecoveryCodesRemaining != 0 {
		t.Fatal("partial enrollment survived rollback", status, err)
	}
	var version int
	if err = s.DB.QueryRow(ctx, `SELECT security_version FROM users WHERE id=$1`, id).Scan(&version); err != nil || version != 0 {
		t.Fatal("version changed on rollback", err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_mfa_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmMFA(ctx, id, hashSecurity(token), code, 0, key); err != nil {
		t.Fatal("same pending proof could not recover", err)
	}
}
func TestMFAConsoleResetRevokesSessionsAndRetainsAuditedHostBoundary(t *testing.T) {
	s, id, token, key := mfaFixture(t)
	ctx := context.Background()
	_, confirmed := mfaEnroll(t, s, id, token, key)
	if err := s.ResetStaffMFA(ctx, id, id); err != nil {
		t.Fatal(err)
	}
	status, err := s.MFAStatus(ctx, id, hashSecurity(confirmed.SessionToken))
	if err != nil || status.Enabled || status.VerifiedUntil != nil {
		t.Fatal(status, err)
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='account.mfa_host_reset' AND actor_id=$1 AND resource_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("host recovery not audited", err)
	}
}
