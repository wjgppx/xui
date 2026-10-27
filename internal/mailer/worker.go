package mailer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/store"
)

var errEmpty = errors.New("no pending mail")

func processOne(ctx context.Context, s *store.Store, sender Sender) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var message Message
	err = tx.QueryRow(ctx, `SELECT m.id,m.to_email,m.subject,m.body FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE m.sent_at IS NULL AND m.cancelled_at IS NULL AND m.dead_lettered_at IS NULL AND m.next_attempt_at<=clock_timestamp() AND t.consumed_at IS NULL AND t.expires_at>clock_timestamp() ORDER BY m.created_at,m.id FOR UPDATE OF m SKIP LOCKED LIMIT 1`).Scan(&message.ID, &message.To, &message.Subject, &message.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errEmpty
	}
	if err != nil {
		return false, err
	}
	// Hold only the mail row while sending. Account token/user rows are not locked,
	// avoiding a cycle with reset/confirm transactions that cancel pending mail.
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sendErr := sender.Send(sendCtx, message)
	cancel()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if sendErr != nil {
		_, err = tx.Exec(ctx, `UPDATE mail_outbox SET attempts=attempts+1,last_error='smtp_failed',next_attempt_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(attempts+1,8))::integer)),dead_lettered_at=CASE WHEN attempts+1>=5 THEN now() ELSE NULL END WHERE id=$1`, message.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE mail_outbox SET sent_at=now(),body='',last_error='' WHERE id=$1`, message.ID)
	}
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return sendErr == nil, nil
}

// Process makes bounded progress. External SMTP acceptance and the database
// commit cannot be atomic: uncertain outcomes may duplicate the same Message-ID.
func Process(ctx context.Context, s *store.Store, sender Sender, limit int) (int, error) {
	if sender == nil || limit < 1 || limit > 100 {
		return 0, errors.New("invalid mail worker configuration")
	}
	// Retired tokens and completed mail should not retain plaintext links. The
	// SKIP LOCKED subquery does not wait on another worker's network operation.
	_, err := s.DB.Exec(ctx, `WITH stale AS (
 SELECT m.id FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash
 WHERE ((m.sent_at IS NOT NULL OR m.cancelled_at IS NOT NULL) AND m.body<>'')
 OR (m.sent_at IS NULL AND m.cancelled_at IS NULL AND (t.consumed_at IS NOT NULL OR t.expires_at<=clock_timestamp()))
 ORDER BY m.id FOR UPDATE OF m SKIP LOCKED LIMIT 100
 ) UPDATE mail_outbox SET body='',cancelled_at=CASE WHEN sent_at IS NULL THEN COALESCE(cancelled_at,clock_timestamp()) ELSE cancelled_at END WHERE id IN(SELECT id FROM stale)`)
	if err != nil {
		return 0, err
	}
	completed := 0
	for i := 0; i < limit; i++ {
		delivered, err := processOne(ctx, s, sender)
		if errors.Is(err, errEmpty) {
			return completed, nil
		}
		if err != nil {
			return completed, err
		}
		if delivered {
			completed++
		}
	}
	return completed, nil
}
func Run(ctx context.Context, s *store.Store, sender Sender) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := Process(ctx, s, sender, 10); err != nil && ctx.Err() == nil {
			slog.Warn("account mail delivery unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
