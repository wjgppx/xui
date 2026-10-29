package mailer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type senderFunc func(context.Context, Message) error

func (f senderFunc) Send(ctx context.Context, message Message) error { return f(ctx, message) }

func queuedMail(t *testing.T, s *store.Store) string {
	t.Helper()
	user := testutil.User(t, s)
	if err := s.IssueAccountToken(context.Background(), user, "verify", "https://portal.example.test"); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := s.DB.QueryRow(context.Background(), `SELECT m.id FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE t.user_id=$1`, user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMailWorkerDeliversOverVerifiedSMTPAndScrubsBody(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	id := queuedMail(t, s)
	fixture := newSMTPFixture(t, true, false)
	sender, err := New(Config{Address: fixture.address, ServerName: "smtp.test", From: "noreply@example.test", Username: "smtp-user", Password: "smtp-password", RootCAs: fixture.roots})
	if err != nil {
		t.Fatal(err)
	}
	if delivered, err := Process(ctx, s, sender, 10); err != nil || delivered != 1 {
		t.Fatal("mail delivery failed", delivered, err)
	}
	select {
	case data := <-fixture.messages:
		if !strings.Contains(string(data), "Message-ID: <"+id+"@example.test>") {
			t.Fatal("outbox identity not preserved in SMTP")
		}
	default:
		t.Fatal("mail not accepted by SMTP fixture")
	}
	var body, lastError string
	var sentAt, cancelledAt *time.Time
	var attempts int
	if err := s.DB.QueryRow(ctx, `SELECT body,last_error,sent_at,cancelled_at,attempts FROM mail_outbox WHERE id=$1`, id).Scan(&body, &lastError, &sentAt, &cancelledAt, &attempts); err != nil {
		t.Fatal(err)
	}
	if body != "" || lastError != "" || sentAt == nil || cancelledAt != nil || attempts != 0 {
		t.Fatal("successful delivery state not recorded or plaintext retained")
	}
	if delivered, err := Process(ctx, s, sender, 10); err != nil || delivered != 0 {
		t.Fatal("delivered mail resent", delivered, err)
	}
	if fixture.accepts.Load() != 1 {
		t.Fatal("delivered row opened another SMTP session")
	}
}

func TestMailWorkerRetryBackoffDeadLetterAndStableIdentity(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	id := queuedMail(t, s)
	calls := 0
	sender := senderFunc(func(ctx context.Context, message Message) error {
		calls++
		if message.ID != id || !strings.Contains(message.Body, "account-action=verify") {
			t.Error("retry changed mail identity or lost action link")
		}
		return errors.New("sensitive SMTP server response and mail token")
	})
	for attempt := 1; attempt <= 5; attempt++ {
		if delivered, err := Process(ctx, s, sender, 10); err != nil || delivered != 0 {
			t.Fatal("transport failure must be persisted for retry", delivered, err)
		}
		var attempts int
		var lastError string
		var dead *time.Time
		var next time.Time
		if err := s.DB.QueryRow(ctx, `SELECT attempts,last_error,dead_lettered_at,next_attempt_at FROM mail_outbox WHERE id=$1`, id).Scan(&attempts, &lastError, &dead, &next); err != nil {
			t.Fatal(err)
		}
		if attempts != attempt || calls != attempt || lastError != "smtp_failed" || !next.After(time.Now()) || (dead != nil) != (attempt == 5) {
			t.Fatal("retry state invalid", attempts, calls, lastError, dead)
		}
		// A second pass before the due time must not immediately redeliver.
		if delivered, err := Process(ctx, s, sender, 10); err != nil || delivered != 0 || calls != attempt {
			t.Fatal("backoff ignored", delivered, calls, err)
		}
		if _, err := s.DB.Exec(ctx, `UPDATE mail_outbox SET next_attempt_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if delivered, err := Process(ctx, s, sender, 10); err != nil || delivered != 0 || calls != 5 {
		t.Fatal("dead letter retried automatically", delivered, calls, err)
	}
	// A failed transport can retain a usable link only until its token expires.
	if _, err := s.DB.Exec(ctx, `UPDATE account_tokens SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE token_hash=(SELECT token_hash FROM mail_outbox WHERE id=$1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := Process(ctx, s, sender, 10); err != nil {
		t.Fatal(err)
	}
	var body string
	var cancelled *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT body,cancelled_at FROM mail_outbox WHERE id=$1`, id).Scan(&body, &cancelled); err != nil {
		t.Fatal(err)
	}
	if body != "" || cancelled == nil || calls != 5 {
		t.Fatal("expired dead letter retained token or reached SMTP")
	}
}

func TestMailWorkersSkipLockedRowsWithoutDuplicateDelivery(t *testing.T) {
	s := testutil.Database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, second := queuedMail(t, s), queuedMail(t, s)
	started := make(chan string, 1)
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		delivered, err := Process(ctx, s, senderFunc(func(ctx context.Context, message Message) error {
			started <- message.ID
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}), 1)
		if err == nil && delivered != 1 {
			err = errors.New("first worker failed to deliver")
		}
		result <- err
	}()
	var held string
	select {
	case held = <-started:
	case <-ctx.Done():
		t.Fatal("first worker did not start")
	}
	var other string
	delivered, err := Process(ctx, s, senderFunc(func(ctx context.Context, message Message) error { other = message.ID; return nil }), 1)
	close(release)
	if err != nil || delivered != 1 || other == held || (other != first && other != second) {
		t.Fatal("second worker did not skip the in-flight row", delivered, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_outbox WHERE sent_at IS NOT NULL AND body=''`).Scan(&count); err != nil || count != 2 {
		t.Fatal("concurrent delivery state invalid", count, err)
	}
}

func TestMailWorkerNeverSendsRetiredTokensAndScrubsCompletedRows(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	expired, consumed, cancelled, sent := queuedMail(t, s), queuedMail(t, s), queuedMail(t, s), queuedMail(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE account_tokens SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE token_hash=(SELECT token_hash FROM mail_outbox WHERE id=$1)`, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE account_tokens SET consumed_at=now() WHERE token_hash=(SELECT token_hash FROM mail_outbox WHERE id=$1)`, consumed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE mail_outbox SET cancelled_at=now() WHERE id=$1`, cancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE mail_outbox SET sent_at=now() WHERE id=$1`, sent); err != nil {
		t.Fatal(err)
	}
	sender := senderFunc(func(context.Context, Message) error { t.Error("retired mail reached transport"); return nil })
	if delivered, err := Process(ctx, s, sender, 10); err != nil || delivered != 0 {
		t.Fatal("retired mail delivery attempt", delivered, err)
	}
	var bodies, cancelledCount int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE body<>''),count(*) FILTER (WHERE cancelled_at IS NOT NULL) FROM mail_outbox`).Scan(&bodies, &cancelledCount); err != nil {
		t.Fatal(err)
	}
	if bodies != 0 || cancelledCount != 3 {
		t.Fatal("retired action links not scrubbed or sent row marked cancelled", bodies, cancelledCount)
	}
}

func TestMailWorkerCancellationLeavesUncommittedMailRetryable(t *testing.T) {
	s := testutil.Database(t)
	id := queuedMail(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sender := senderFunc(func(ctx context.Context, message Message) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if delivered, err := Process(ctx, s, sender, 1); !errors.Is(err, context.DeadlineExceeded) || delivered != 0 {
		t.Fatal("cancellation not propagated", delivered, err)
	}
	var attempts int
	var sent *time.Time
	if err := s.DB.QueryRow(context.Background(), `SELECT attempts,sent_at FROM mail_outbox WHERE id=$1`, id).Scan(&attempts, &sent); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || sent != nil {
		t.Fatal("cancellation left a partial transaction")
	}
	if delivered, err := Process(context.Background(), s, senderFunc(func(context.Context, Message) error { return nil }), 1); err != nil || delivered != 1 {
		t.Fatal("cancelled mail not retryable", delivered, err)
	}
}

func TestMailWorkerAcceptedMailWithFailedCommitRetriesSameIdentity(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	id := queuedMail(t, s)
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_mail_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.sent_at IS NOT NULL THEN RAISE EXCEPTION 'injected completion failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_mail_completion BEFORE UPDATE ON mail_outbox FOR EACH ROW EXECUTE FUNCTION reject_mail_completion()`); err != nil {
		t.Fatal(err)
	}
	var identities []string
	sender := senderFunc(func(ctx context.Context, message Message) error {
		identities = append(identities, message.ID)
		return nil
	})
	if delivered, err := Process(ctx, s, sender, 1); err == nil || delivered != 0 {
		t.Fatal("completion failure hidden", delivered, err)
	}
	if _, err := s.DB.Exec(ctx, `DROP TRIGGER reject_mail_completion ON mail_outbox`); err != nil {
		t.Fatal(err)
	}
	if delivered, err := Process(ctx, s, sender, 1); err != nil || delivered != 1 {
		t.Fatal("uncertain delivery not retryable", delivered, err)
	}
	if len(identities) != 2 || identities[0] != id || identities[1] != id {
		t.Fatal("uncertain delivery changed its Message-ID")
	}
}
