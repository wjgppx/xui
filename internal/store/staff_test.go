package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func makeStaffAdmin(t *testing.T, s *store.Store) string {
	t.Helper()
	id := testutil.User(t, s)
	if _, err := s.DB.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStaffConcurrentCrossDemotionsPreserveAdministrator(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	a, b := makeStaffAdmin(t, s), makeStaffAdmin(t, s)
	for _, user := range []string{a, b} {
		if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, store.ID()+store.ID(), user); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		wg.Add(1)
		go func(actor, target string) {
			defer wg.Done()
			<-start
			results <- s.SetUserRole(ctx, actor, target, "support", false)
		}(pair[0], pair[1])
	}
	close(start)
	wg.Wait()
	close(results)
	var successes, rejected int
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, store.ErrRoleConflict) || errors.Is(err, store.ErrRoleForbidden) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	var admins, audits, invalidated int
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE role='admin'),(SELECT count(*) FROM audit_events WHERE action='staff.role_changed'),(SELECT count(*) FROM users u WHERE u.role='support' AND u.security_version=1 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.user_id=u.id))`).Scan(&admins, &audits, &invalidated); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || rejected != 1 || admins != 1 || audits != 1 || invalidated != 1 {
		t.Fatalf("cross demotions violated role isolation: successes=%d rejected=%d admins=%d audits=%d invalidated=%d", successes, rejected, admins, audits, invalidated)
	}
}

func TestStaffRoleChangeRechecksActorAfterRoleLock(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, target := makeStaffAdmin(t, s), testutil.User(t, s)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(738912409)`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.SetUserRole(ctx, actor, target, "admin", false) }()
	// This models a competing role change completing while a request is queued.
	if _, err = tx.Exec(ctx, `UPDATE users SET role='support',security_version=security_version+1 WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatalf("demoted actor granted new administrator: %v", err)
	}
	var role string
	var version, audits int
	if err = s.DB.QueryRow(ctx, `SELECT role,security_version,(SELECT count(*) FROM audit_events WHERE resource_id=$1) FROM users WHERE id=$1`, target).Scan(&role, &version, &audits); err != nil {
		t.Fatal(err)
	}
	if role != "user" || version != 0 || audits != 0 {
		t.Fatal("rejected role change mutated target")
	}
}

func TestStaffRoleRecoveryInvalidationAndNoop(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	admin := makeStaffAdmin(t, s)
	if err := s.SetUserRole(ctx, admin, admin, "support", false); !errors.Is(err, store.ErrRoleConflict) {
		t.Fatalf("self/final administrator demotion accepted: %v", err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, store.ID()+store.ID(), admin); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(ctx, admin, admin, "admin", false); err != nil {
		t.Fatal("same-role update failed", err)
	}
	var version, sessions, audits int
	if err := s.DB.QueryRow(ctx, `SELECT security_version,(SELECT count(*) FROM sessions WHERE user_id=$1),(SELECT count(*) FROM audit_events WHERE resource_id=$1) FROM users WHERE id=$1`, admin).Scan(&version, &sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if version != 0 || sessions != 1 || audits != 0 {
		t.Fatal("same-role update needlessly invalidated session")
	}
	if err := s.SetUserRole(ctx, admin, admin, "user", true); err != nil {
		t.Fatal("local recovery cannot demote final administrator", err)
	}
	var role string
	if err := s.DB.QueryRow(ctx, `SELECT role,security_version,(SELECT count(*) FROM sessions WHERE user_id=$1),(SELECT count(*) FROM audit_events WHERE resource_id=$1 AND action='staff.role_changed_via_cli') FROM users WHERE id=$1`, admin).Scan(&role, &version, &sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if role != "user" || version != 1 || sessions != 0 || audits != 1 {
		t.Fatalf("local role change not securely applied: %s %d %d %d", role, version, sessions, audits)
	}
	if err := s.SetUserRole(ctx, admin, admin, "admin", true); err != nil {
		t.Fatal("local recovery cannot bootstrap administrator", err)
	}
	if err := s.SetUserRole(ctx, admin, store.ID(), "user", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user not rejected: %v", err)
	}
	if err := s.SetUserRole(ctx, admin, admin, "superadmin", true); !errors.Is(err, store.ErrRoleConflict) {
		t.Fatalf("invalid role not rejected: %v", err)
	}
}
