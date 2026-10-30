package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func TestStaffWebRoleChangeRequiresCurrentSessionAndStepUp(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	for _, item := range []struct {
		name, mutation           string
		requireMFA, requireEmail bool
		want                     error
	}{
		{name: "optional unenrolled session"},
		{name: "password or MFA generation changed", mutation: `UPDATE users SET security_version=security_version+1 WHERE id=$1`, want: store.ErrRoleSessionInvalid},
		{name: "logout removed session", mutation: `DELETE FROM sessions WHERE user_id=$1`, want: store.ErrRoleSessionInvalid},
		{name: "login expired", mutation: `UPDATE sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, want: store.ErrRoleSessionInvalid},
		{name: "leftover old session generation", mutation: `UPDATE sessions SET security_version=security_version+1 WHERE user_id=$1`, want: store.ErrRoleSessionInvalid},
		{name: "required enrollment missing", requireMFA: true, want: store.ErrRoleMFAEnrollmentRequired},
		{name: "enrolled factor needs stepup even when optional", mutation: `INSERT INTO staff_mfa(user_id,secret_cipher,enabled_at) VALUES($1,'\x01',clock_timestamp())`, want: store.ErrRoleMFARequired},
		{name: "expired stepup", mutation: `WITH factor AS (INSERT INTO staff_mfa(user_id,secret_cipher,enabled_at) VALUES($1,'\x01',clock_timestamp())) UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, requireMFA: true, want: store.ErrRoleMFARequired},
		{name: "current factor and stepup", mutation: `WITH factor AS (INSERT INTO staff_mfa(user_id,secret_cipher,enabled_at) VALUES($1,'\x01',clock_timestamp())) UPDATE sessions SET mfa_verified_at=clock_timestamp() WHERE user_id=$1`, requireMFA: true},
		{name: "production email missing", requireEmail: true, want: store.ErrEmailUnverified},
		{name: "production email current", mutation: `UPDATE users SET email_verified_at=clock_timestamp() WHERE id=$1`, requireEmail: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			actor, target := makeStaffAdmin(t, s), testutil.User(t, s)
			hash := store.ID() + store.ID()
			if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,security_version) VALUES($1,$2,clock_timestamp()+interval '1 hour',0)`, hash, actor); err != nil {
				t.Fatal(err)
			}
			if item.mutation != "" {
				if _, err := s.DB.Exec(ctx, item.mutation, actor); err != nil {
					t.Fatal(err)
				}
			}
			policy := *s
			policy.RequireVerified = item.requireEmail
			err := policy.SetUserRoleForSession(ctx, actor, target, "admin", hash, 0, item.requireMFA)
			if !errors.Is(err, item.want) {
				t.Fatalf("unexpected session authorization: %v", err)
			}
			var role string
			var version, audits int
			if err := s.DB.QueryRow(ctx, `SELECT role,security_version,(SELECT count(*) FROM audit_events WHERE resource_id=$1) FROM users WHERE id=$1`, target).Scan(&role, &version, &audits); err != nil {
				t.Fatal(err)
			}
			if item.want == nil {
				if role != "admin" || version != 1 || audits != 1 {
					t.Fatal("valid session did not atomically grant role and audit")
				}
			} else if role != "user" || version != 0 || audits != 0 {
				t.Fatal("rejected session changed target or audit")
			}
		})
	}
}

func TestStaffRoleSessionExpiryIsCheckedAfterSessionAndTargetLockWaits(t *testing.T) {
	s := testutil.Database(t)
	for _, item := range []struct {
		name, lockTarget, expiresField string
		want                           error
	}{
		{"session lock login expiry", "session", "expires_at", store.ErrRoleSessionInvalid},
		{"session lock stepup expiry", "session", "mfa_verified_at+interval '15 minutes'", store.ErrRoleMFARequired},
		{"target lock login expiry", "target", "expires_at", store.ErrRoleSessionInvalid},
		{"target lock stepup expiry", "target", "mfa_verified_at+interval '15 minutes'", store.ErrRoleMFARequired},
	} {
		t.Run(item.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			actor, target := makeStaffAdmin(t, s), testutil.User(t, s)
			hash := store.ID() + store.ID()
			if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,security_version,mfa_verified_at) VALUES($1,$2,clock_timestamp()+interval '1 hour',0,clock_timestamp())`, hash, actor); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(ctx, `INSERT INTO staff_mfa(user_id,secret_cipher,enabled_at) VALUES($1,'\x01',clock_timestamp())`, actor); err != nil {
				t.Fatal(err)
			}
			lock, err := s.DB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(context.Background())
			if item.expiresField == "expires_at" {
				_, err = lock.Exec(ctx, `UPDATE sessions SET expires_at=clock_timestamp()+interval '1 second' WHERE user_id=$1`, actor)
			} else {
				_, err = lock.Exec(ctx, `UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '15 minutes'+interval '1 second' WHERE user_id=$1`, actor)
			}
			if err != nil {
				t.Fatal(err)
			}
			if item.lockTarget == "target" {
				// Commit the short authorization lifetime before blocking the
				// target. The actor/session locks then cannot renew that lifetime.
				if err = lock.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				lock, err = s.DB.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback(context.Background())
				if _, err = lock.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, target); err != nil {
					t.Fatal(err)
				}
			}
			cfg := s.DB.Config()
			trace := "role-expiry-" + store.ID()
			cfg.ConnConfig.RuntimeParams["application_name"] = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			policy := *s
			policy.DB = pool
			done := make(chan error, 1)
			go func() { done <- policy.SetUserRoleForSession(ctx, actor, target, "admin", hash, 0, true) }()
			query := "%FROM sessions WHERE token_hash=%FOR UPDATE"
			if item.lockTarget == "target" {
				query = "%SELECT role FROM users WHERE id=%FOR NO KEY UPDATE"
			}
			for {
				var waiting bool
				if err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE $2)`, trace, query).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case got := <-done:
					t.Fatalf("operation did not wait at expected lock: %v", got)
				case <-ctx.Done():
					t.Fatal("operation did not reach expected lock")
				case <-time.After(5 * time.Millisecond):
				}
			}
			// Wait for the database's actual lifetime boundary, rather than
			// assuming a sleep established either the lock or the expiry.
			for {
				var expired bool
				if err = lock.QueryRow(ctx, `SELECT clock_timestamp()>=`+item.expiresField+` FROM sessions WHERE user_id=$1`, actor).Scan(&expired); err != nil {
					t.Fatal(err)
				}
				if expired {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("session lifetime did not expire")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if err = lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if !errors.Is(got, item.want) {
					t.Fatalf("expired role operation returned %v", got)
				}
			case <-ctx.Done():
				t.Fatal("expired role operation did not finish")
			}
			var role string
			var version, audits int
			if err = s.DB.QueryRow(ctx, `SELECT role,security_version,(SELECT count(*) FROM audit_events WHERE resource_id=$1) FROM users WHERE id=$1`, target).Scan(&role, &version, &audits); err != nil {
				t.Fatal(err)
			}
			if role != "user" || version != 0 || audits != 0 {
				t.Fatal("expired role grant escaped rollback")
			}
		})
	}
}
