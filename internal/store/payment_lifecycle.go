package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type PaymentJob struct {
	ID, PaymentID, OrderID, Provider, UserID, PlanName, Currency, ClaimToken string
	Amount                                                                   int64
	Attempts, Checks                                                         int
}

// SchedulePaymentReconciliation is bounded so a large overdue backlog cannot
// hold all order rows or monopolize a control process. Expiry stops new checkout
// creation; it is not proof that a remote payment has closed.
func (s *Store) SchedulePaymentReconciliation(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `WITH due AS (
 SELECT id FROM orders WHERE status='pending' AND expires_at<=clock_timestamp()
 ORDER BY expires_at LIMIT 500 FOR NO KEY UPDATE SKIP LOCKED
 ) UPDATE orders SET status='expired' WHERE id IN(SELECT id FROM due)`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_reconciliation(payment_id,next_attempt_at,horizon_at)
 SELECT p.id,p.created_at+interval '30 seconds',p.created_at+interval '7 days'
 FROM payment_intents p JOIN orders o ON o.id=p.order_id
 WHERE p.provider IN ('alipay','wechat') AND p.status<>'paid' AND o.status IN ('pending','expired')
 AND NOT EXISTS(SELECT 1 FROM payment_reconciliation j WHERE j.payment_id=p.id)
 ORDER BY p.created_at,p.id LIMIT 500 ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH done AS (
 SELECT j.payment_id FROM payment_reconciliation j JOIN payment_intents p ON p.id=j.payment_id
 WHERE p.status='paid' AND j.status<>'paid' AND (j.lease_until IS NULL OR j.lease_until<=clock_timestamp()) ORDER BY j.payment_id LIMIT 500 FOR UPDATE OF j SKIP LOCKED
 ) UPDATE payment_reconciliation SET status='paid',last_outcome='paid',claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp()
 WHERE payment_id IN(SELECT payment_id FROM done)`)
	if err != nil {
		return err
	}
	// A finite horizon stops unbounded provider traffic. Only an explicit staff
	// retry reopens this horizon; it never closes the financial order itself.
	rows, err := tx.Query(ctx, `WITH due AS (
 SELECT payment_id FROM payment_reconciliation WHERE status IN ('queued','waiting','processing')
 AND horizon_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp())
 ORDER BY horizon_at LIMIT 500 FOR UPDATE SKIP LOCKED
 ) UPDATE payment_reconciliation SET status='manual_required',last_outcome='manual_required',claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp()
 WHERE payment_id IN(SELECT payment_id FROM due) RETURNING payment_id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = recordPaymentActivityTx(ctx, tx, id, "", "auto_query", "manual_required", true); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Claim leases a job without holding a PostgreSQL row lock across network I/O.
// The token prevents an old worker from completing a claim that another worker
// has recovered after expiry.
func (s *Store) ClaimPaymentReconciliation(ctx context.Context) (PaymentJob, error) {
	var j PaymentJob
	token := ID()
	err := s.DB.QueryRow(ctx, `WITH candidate AS (
 SELECT j.payment_id FROM payment_reconciliation j
 JOIN payment_intents p ON p.id=j.payment_id JOIN orders o ON o.id=p.order_id
 WHERE j.status IN ('queued','waiting','processing') AND j.next_attempt_at<=clock_timestamp()
 AND (j.lease_until IS NULL OR j.lease_until<=clock_timestamp()) AND j.horizon_at>clock_timestamp()
 AND p.status<>'paid' AND o.status IN ('pending','expired')
 ORDER BY j.next_attempt_at,j.payment_id FOR UPDATE OF j SKIP LOCKED LIMIT 1
 ), claimed AS (
 UPDATE payment_reconciliation SET status='processing',checks=checks+1,claim_token=$1,
 lease_until=clock_timestamp()+interval '30 seconds',updated_at=clock_timestamp()
 WHERE payment_id IN(SELECT payment_id FROM candidate) RETURNING *
 ) SELECT c.payment_id,p.order_id,p.provider,o.user_id,o.plan_name,o.price_minor,o.currency,c.attempts,c.checks,c.claim_token
 FROM claimed c JOIN payment_intents p ON p.id=c.payment_id JOIN orders o ON o.id=p.order_id`, token).
		Scan(&j.PaymentID, &j.OrderID, &j.Provider, &j.UserID, &j.PlanName, &j.Amount, &j.Currency, &j.Attempts, &j.Checks, &j.ClaimToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFound
	}
	j.ID = j.PaymentID
	return j, err
}

func reconciliationOutcome(outcome string) bool {
	switch outcome {
	case "pending", "not_paid", "paid", "gateway_error", "verification_error", "storage_error", "conflict", "provider_unavailable":
		return true
	}
	return false
}
func (s *Store) FinishPaymentReconciliation(ctx context.Context, paymentID, claimToken, outcome string) error {
	if !reconciliationOutcome(outcome) {
		return ErrPaymentConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var attempts, checks int
	err = tx.QueryRow(ctx, `SELECT attempts,checks FROM payment_reconciliation WHERE payment_id=$1 AND claim_token=$2
 AND status='processing' AND lease_until>clock_timestamp() FOR UPDATE`, paymentID, claimToken).Scan(&attempts, &checks)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPaymentConflict
	}
	if err != nil {
		return err
	}
	var paid bool
	if err = tx.QueryRow(ctx, `SELECT status='paid' FROM payment_intents WHERE id=$1`, paymentID).Scan(&paid); err != nil {
		return err
	}
	status := "waiting"
	// A callback may have won while the worker received a pending result.
	if paid {
		outcome = "paid"
		status = "paid"
	} else if outcome == "paid" {
		return ErrPaymentConflict
	}
	if outcome != "pending" && outcome != "not_paid" && outcome != "paid" {
		attempts++
	} else {
		attempts = 0
	}
	if attempts >= 8 && status != "paid" {
		status = "manual_required"
	}
	delay := 30
	// Pending transactions use check count to back off but do not consume the
	// failure budget. Failed attempts have a separate bounded exponential delay.
	exponent := checks
	if outcome != "pending" && outcome != "not_paid" {
		exponent = attempts
	}
	for i := 1; i < exponent && delay < 1800; i++ {
		delay *= 2
	}
	if delay > 1800 {
		delay = 1800
	}
	_, err = tx.Exec(ctx, `UPDATE payment_reconciliation SET status=$2,attempts=$3,last_outcome=$4,
 next_attempt_at=clock_timestamp()+make_interval(secs=>$5),claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp()
 WHERE payment_id=$1`, paymentID, status, attempts, outcome, delay)
	if err != nil {
		return err
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, "", "auto_query", outcome, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func paymentActivitySource(source string) bool {
	switch source {
	case "create", "notify", "query", "auto_query", "close", "retry", "refund", "gateway_close":
		return true
	}
	return false
}
func paymentActivityOutcome(outcome string) bool {
	switch outcome {
	case "created", "ready", "paid", "pending", "not_paid", "gateway_error", "verification_error", "storage_error", "conflict", "provider_unavailable", "closed", "retry_scheduled", "manual_required", "requested", "approved", "submitted", "succeeded", "failed", "not_found", "unsupported":
		return true
	}
	return false
}
func recordPaymentActivityTx(ctx context.Context, tx pgx.Tx, paymentID, provider, source, outcome string, deriveProvider bool) error {
	if !paymentActivitySource(source) || !paymentActivityOutcome(outcome) || (provider != "" && !paymentProvider(provider)) {
		return ErrPaymentConflict
	}
	_, err := tx.Exec(ctx, `INSERT INTO payment_activity(payment_id,order_id,provider,source,outcome)
 SELECT p.id,p.order_id,CASE WHEN $5 THEN COALESCE(p.provider,$2) ELSE $2 END,$3,$4
 FROM (SELECT 1) singleton LEFT JOIN payment_intents p ON p.id=$1 AND ($5 OR p.provider=$2)`, paymentID, provider, source, outcome, deriveProvider)
	return err
}
func (s *Store) RecordPaymentActivity(ctx context.Context, paymentID, provider, source, outcome string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = recordPaymentActivityTx(ctx, tx, paymentID, provider, source, outcome, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CloseOrder(ctx context.Context, userID, orderID string) (Order, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, err
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 AND user_id=$2 FOR NO KEY UPDATE`, orderID, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if status == "closed" {
		if err = tx.Commit(ctx); err != nil {
			return Order{}, err
		}
		return s.PaymentOrder(ctx, userID, orderID)
	}
	if status != "pending" && status != "expired" {
		return Order{}, ErrPaymentConflict
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_intents WHERE order_id=$1)`, orderID).Scan(&exists); err != nil {
		return Order{}, err
	}
	if exists {
		return Order{}, ErrPaymentConflict
	}
	_, err = tx.Exec(ctx, `UPDATE orders SET status='closed',closed_at=clock_timestamp() WHERE id=$1`, orderID)
	if err != nil {
		return Order{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_activity(order_id,provider,source,outcome) VALUES($1,'','close','closed')`, orderID)
	if err != nil {
		return Order{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'order.closed',$2)`, userID, orderID)
	if err != nil {
		return Order{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	return s.PaymentOrder(ctx, userID, orderID)
}

type PaymentOverview struct {
	ID                string     `json:"id"`
	OrderID           string     `json:"order_id"`
	UserEmail         string     `json:"user_email"`
	Provider          string     `json:"provider"`
	Status            string     `json:"status"`
	Amount            int64      `json:"amount_minor"`
	Currency          string     `json:"currency"`
	TransactionID     *string    `json:"transaction_id"`
	CreatedAt         time.Time  `json:"created_at"`
	PaidAt            *time.Time `json:"paid_at"`
	OrderStatus       string     `json:"order_status"`
	ExpiresAt         time.Time  `json:"expires_at"`
	ReconcileStatus   string     `json:"reconcile_status"`
	ReconcileAttempts int        `json:"reconcile_attempts"`
	LastOutcome       string     `json:"last_outcome"`
	NextCheckAt       *time.Time `json:"next_check_at"`
}

func (s *Store) ListPayments(ctx context.Context, status, provider, orderID string) ([]PaymentOverview, error) {
	if provider != "" && !paymentProvider(provider) {
		return nil, ErrPaymentConflict
	}
	if status != "" && status != "pending" && status != "ready" && status != "paid" && status != "manual_required" {
		return nil, ErrPaymentConflict
	}
	rows, err := s.DB.Query(ctx, `SELECT p.id,p.order_id,u.email,p.provider,p.status,o.price_minor,o.currency,p.transaction_id,p.created_at,p.paid_at,
 CASE WHEN o.status='pending' AND o.expires_at<=clock_timestamp() THEN 'expired' ELSE o.status END,o.expires_at,
 CASE WHEN p.status='paid' THEN 'paid' WHEN o.status='closed' THEN 'closed' WHEN o.status='closing' THEN COALESCE(c.status,'queued') ELSE COALESCE(j.status,CASE WHEN p.provider IN ('epay_alipay','epay_wechat') THEN 'waiting_callback' ELSE 'queued' END) END,
 CASE WHEN o.status='closing' THEN COALESCE(c.attempts,0) ELSE COALESCE(j.attempts,0) END,CASE WHEN o.status='closing' THEN COALESCE(c.last_outcome,'') ELSE COALESCE(j.last_outcome,'') END,CASE WHEN p.status='paid' OR o.status='closed' THEN NULL WHEN o.status='closing' AND c.status IN ('queued','waiting') THEN c.next_attempt_at WHEN o.status IN ('pending','expired') AND j.status IN ('queued','waiting') THEN j.next_attempt_at ELSE NULL END
 FROM payment_intents p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.user_id LEFT JOIN payment_reconciliation j ON j.payment_id=p.id LEFT JOIN payment_action_jobs c ON c.payment_id=p.id AND c.kind='close'
 WHERE ($1='' OR p.status=$1 OR ($1='manual_required' AND p.status<>'paid' AND ((o.status='closing' AND c.status='manual_required') OR (o.status IN ('pending','expired') AND j.status='manual_required')))) AND ($2='' OR p.provider=$2) AND ($3='' OR p.order_id=$3)
 ORDER BY p.created_at DESC,p.id DESC LIMIT 100`, status, provider, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaymentOverview{}
	for rows.Next() {
		var p PaymentOverview
		if err = rows.Scan(&p.ID, &p.OrderID, &p.UserEmail, &p.Provider, &p.Status, &p.Amount, &p.Currency, &p.TransactionID, &p.CreatedAt, &p.PaidAt, &p.OrderStatus, &p.ExpiresAt, &p.ReconcileStatus, &p.ReconcileAttempts, &p.LastOutcome, &p.NextCheckAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type PaymentActivity struct {
	ID        int64     `json:"id"`
	PaymentID *string   `json:"payment_id"`
	OrderID   *string   `json:"order_id"`
	Provider  string    `json:"provider"`
	Source    string    `json:"source"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) PaymentActivity(ctx context.Context, paymentID string) ([]PaymentActivity, error) {
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_intents WHERE id=$1)`, paymentID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT id,payment_id,order_id,provider,source,outcome,created_at FROM payment_activity WHERE payment_id=$1 ORDER BY id DESC LIMIT 100`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaymentActivity{}
	for rows.Next() {
		var a PaymentActivity
		if err = rows.Scan(&a.ID, &a.PaymentID, &a.OrderID, &a.Provider, &a.Source, &a.Outcome, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) RetryPaymentReconciliation(ctx context.Context, actorID, paymentID string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR SHARE`, actorID).Scan(&role)
	if err != nil {
		return err
	}
	if role != "admin" && role != "finance" {
		return ErrRoleForbidden
	}
	var provider, status, orderStatus string
	err = tx.QueryRow(ctx, `SELECT p.provider,p.status,o.status FROM payment_intents p JOIN orders o ON o.id=p.order_id WHERE p.id=$1`, paymentID).Scan(&provider, &status, &orderStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if (provider != "alipay" && provider != "wechat") || status == "paid" || (orderStatus != "pending" && orderStatus != "expired") {
		return ErrPaymentConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_reconciliation(payment_id,horizon_at) VALUES($1,clock_timestamp()+interval '7 days') ON CONFLICT DO NOTHING`, paymentID)
	if err != nil {
		return err
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT status='processing' AND lease_until>clock_timestamp() FROM payment_reconciliation WHERE payment_id=$1 FOR UPDATE`, paymentID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrPaymentConflict
	}
	// Recheck after obtaining the job lock; a late callback is never undone.
	if err = tx.QueryRow(ctx, `SELECT status FROM payment_intents WHERE id=$1`, paymentID).Scan(&status); err != nil {
		return err
	}
	if status == "paid" {
		return ErrPaymentConflict
	}
	_, err = tx.Exec(ctx, `UPDATE payment_reconciliation SET status='queued',attempts=0,checks=0,last_outcome='retry_scheduled',claim_token=NULL,lease_until=NULL,next_attempt_at=clock_timestamp(),horizon_at=clock_timestamp()+interval '7 days',updated_at=clock_timestamp() WHERE payment_id=$1`, paymentID)
	if err != nil {
		return err
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, provider, "retry", "retry_scheduled", false); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'payment.reconciliation_retried',$2)`, actorID, paymentID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RecentPaymentActivity(ctx context.Context) ([]PaymentActivity, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,payment_id,order_id,provider,source,outcome,created_at FROM payment_activity ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaymentActivity{}
	for rows.Next() {
		var a PaymentActivity
		if err = rows.Scan(&a.ID, &a.PaymentID, &a.OrderID, &a.Provider, &a.Source, &a.Outcome, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
