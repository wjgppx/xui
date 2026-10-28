package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type Refund struct {
	ID               string     `json:"id"`
	PaymentID        string     `json:"payment_id"`
	OrderID          string     `json:"order_id"`
	Provider         string     `json:"provider"`
	TransactionID    string     `json:"transaction_id"`
	Amount           int64      `json:"amount_minor"`
	Currency         string     `json:"currency"`
	Reason           string     `json:"reason"`
	Status           string     `json:"status"`
	RequestedBy      string     `json:"requested_by"`
	ApprovedBy       *string    `json:"approved_by"`
	ProviderRefundID *string    `json:"provider_refund_id"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}
type RefundOverview struct {
	Refund
	UserEmail string `json:"user_email"`
}

const refundColumns = `id,payment_id,order_id,provider,transaction_id,amount_minor,currency,reason,status,requested_by,approved_by,provider_refund_id,created_at,updated_at,completed_at`

func refundTargets(r *Refund) []any {
	return []any{&r.ID, &r.PaymentID, &r.OrderID, &r.Provider, &r.TransactionID, &r.Amount, &r.Currency, &r.Reason, &r.Status, &r.RequestedBy, &r.ApprovedBy, &r.ProviderRefundID, &r.CreatedAt, &r.UpdatedAt, &r.CompletedAt}
}
func scanRefund(row pgx.Row) (Refund, error) {
	var r Refund
	err := row.Scan(refundTargets(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}
func (s *Store) Refund(ctx context.Context, id string) (Refund, error) {
	return scanRefund(s.DB.QueryRow(ctx, `SELECT `+refundColumns+` FROM payment_refunds WHERE id=$1`, id))
}
func (s *Store) ListRefunds(ctx context.Context) ([]RefundOverview, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+prefixedRefundColumns()+`,u.email FROM payment_refunds r JOIN orders o ON o.id=r.order_id JOIN users u ON u.id=o.user_id ORDER BY r.created_at DESC,r.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RefundOverview{}
	for rows.Next() {
		var r RefundOverview
		if err = rows.Scan(append(refundTargets(&r.Refund), &r.UserEmail)...); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func prefixedRefundColumns() string { return "r." + strings.ReplaceAll(refundColumns, ",", ",r.") }

func validPaymentActionReason(reason string) bool {
	return reason == strings.TrimSpace(reason) && reason != "" && len(reason) <= 256 && utf8.ValidString(reason) && strings.IndexFunc(reason, unicode.IsControl) < 0
}

// Hold the actor's security generation through the commit. This serializes
// approval with role changes, password resets and session revocation.
func actionActor(ctx context.Context, tx pgx.Tx, actor string, version int64, adminOnly bool) error {
	var role string
	var actual int64
	err := tx.QueryRow(ctx, `SELECT role,security_version FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&role, &actual)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRoleForbidden
	}
	if err != nil {
		return err
	}
	if (version >= 0 && version != actual) || (role != "admin" && (adminOnly || role != "finance")) {
		return ErrRoleForbidden
	}
	return nil
}

type actionPayment struct {
	OrderID, UserID, Provider, Status, OrderStatus, TransactionID, Currency string
	Amount                                                                  int64
}

// Order then intent matches settlement's lock order. Actor checks are acquired
// before these locks; no row lock is held during provider network calls.
func lockActionPayment(ctx context.Context, tx pgx.Tx, paymentID string) (actionPayment, error) {
	var p actionPayment
	err := tx.QueryRow(ctx, `SELECT order_id FROM payment_intents WHERE id=$1`, paymentID).Scan(&p.OrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	err = tx.QueryRow(ctx, `SELECT user_id,status,price_minor,currency FROM orders WHERE id=$1 FOR NO KEY UPDATE`, p.OrderID).Scan(&p.UserID, &p.OrderStatus, &p.Amount, &p.Currency)
	if err != nil {
		return p, err
	}
	err = tx.QueryRow(ctx, `SELECT provider,status,COALESCE(transaction_id,'') FROM payment_intents WHERE id=$1 FOR NO KEY UPDATE`, paymentID).Scan(&p.Provider, &p.Status, &p.TransactionID)
	return p, err
}
func actionAudit(ctx context.Context, tx pgx.Tx, actor, action, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,$2,$3)`, actor, action, id)
	return err
}
func (s *Store) RequestRefund(ctx context.Context, actor, paymentID, reason string) (Refund, error) {
	if !validPaymentActionReason(reason) {
		return Refund{}, ErrPaymentConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Refund{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, -1, false); err != nil {
		return Refund{}, err
	}
	p, err := lockActionPayment(ctx, tx, paymentID)
	if err != nil {
		return Refund{}, err
	}
	if p.Status != "paid" || (p.Provider != "alipay" && p.Provider != "wechat") || p.TransactionID == "" {
		return Refund{}, ErrPaymentConflict
	}
	existing, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM payment_refunds WHERE payment_id=$1`, paymentID))
	if err == nil {
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return Refund{}, err
	}
	if p.OrderStatus != "paid" {
		return Refund{}, ErrPaymentConflict
	}
	r, err := scanRefund(tx.QueryRow(ctx, `INSERT INTO payment_refunds(id,payment_id,order_id,provider,transaction_id,amount_minor,currency,reason,requested_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+refundColumns, ID(), paymentID, p.OrderID, p.Provider, p.TransactionID, p.Amount, p.Currency, reason, actor))
	if err != nil {
		return Refund{}, err
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, p.Provider, "refund", "requested", false); err != nil {
		return Refund{}, err
	}
	if err = actionAudit(ctx, tx, actor, "payment.refund_requested", r.ID); err != nil {
		return Refund{}, err
	}
	return r, tx.Commit(ctx)
}
func (s *Store) ApproveRefund(ctx context.Context, actor, refundID string, version int64) error {
	if version < 0 {
		return ErrRoleForbidden
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, version, true); err != nil {
		return err
	}
	var paymentID string
	if err = tx.QueryRow(ctx, `SELECT payment_id FROM payment_refunds WHERE id=$1`, refundID).Scan(&paymentID); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	p, err := lockActionPayment(ctx, tx, paymentID)
	if err != nil {
		return err
	}
	r, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM payment_refunds WHERE id=$1 FOR NO KEY UPDATE`, refundID))
	if err != nil {
		return err
	}
	if r.Status == "queued" || r.Status == "processing" || r.Status == "succeeded" {
		return tx.Commit(ctx)
	}
	if (r.Status != "requested" && r.Status != "manual_required") || p.Status != "paid" || p.OrderStatus != "paid" {
		return ErrPaymentConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_action_jobs(id,kind,payment_id,refund_id,reason) VALUES($1,'refund',$2,$3,$4) ON CONFLICT(refund_id) DO UPDATE SET status='queued',attempts=0,checks=0,last_outcome='approved',next_attempt_at=clock_timestamp(),horizon_at=clock_timestamp()+interval '7 days',updated_at=clock_timestamp() WHERE payment_action_jobs.status='manual_required'`, ID(), paymentID, refundID, r.Reason)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_refunds SET status='queued',approved_by=$2,updated_at=clock_timestamp() WHERE id=$1`, refundID, actor); err != nil {
		return err
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, p.Provider, "refund", "approved", false); err != nil {
		return err
	}
	if err = actionAudit(ctx, tx, actor, "payment.refund_approved", refundID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RequestPaymentClose(ctx context.Context, actor, paymentID, reason string, version int64) error {
	if version < 0 {
		return ErrRoleForbidden
	}
	if !validPaymentActionReason(reason) {
		return ErrPaymentConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, version, false); err != nil {
		return err
	}
	p, err := lockActionPayment(ctx, tx, paymentID)
	if err != nil {
		return err
	}
	if p.Status == "paid" || (p.Provider != "alipay" && p.Provider != "wechat") {
		return ErrPaymentConflict
	}
	var state string
	err = tx.QueryRow(ctx, `SELECT status FROM payment_action_jobs WHERE payment_id=$1 AND kind='close' FOR UPDATE`, paymentID).Scan(&state)
	if err == nil {
		if state != "manual_required" {
			return tx.Commit(ctx)
		}
		if p.OrderStatus != "closing" {
			return ErrPaymentConflict
		}
		_, err = tx.Exec(ctx, `UPDATE payment_action_jobs SET status='queued',attempts=0,checks=0,last_outcome='approved',next_attempt_at=clock_timestamp(),horizon_at=clock_timestamp()+interval '7 days',updated_at=clock_timestamp() WHERE payment_id=$1 AND kind='close'`, paymentID)
	} else if errors.Is(err, pgx.ErrNoRows) {
		if p.OrderStatus != "pending" && p.OrderStatus != "expired" {
			return ErrPaymentConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO payment_action_jobs(id,kind,payment_id,reason) VALUES($1,'close',$2,$3)`, ID(), paymentID, reason)
	} else {
		return err
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET status='closing' WHERE id=$1`, p.OrderID); err != nil {
		return err
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, p.Provider, "gateway_close", "requested", false); err != nil {
		return err
	}
	if err = actionAudit(ctx, tx, actor, "payment.close_requested", paymentID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type ActionJob struct {
	ID, Kind, PaymentID, RefundID, OrderID, Provider, TransactionID, Currency, Reason, ClaimToken string
	Amount                                                                                        int64
	Attempts, Checks                                                                              int
	Submitted                                                                                     bool
}

func (s *Store) ClaimPaymentAction(ctx context.Context) (ActionJob, error) {
	var j ActionJob
	err := s.DB.QueryRow(ctx, `WITH candidate AS (
 SELECT id FROM payment_action_jobs WHERE status IN ('queued','waiting','processing') AND next_attempt_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1
 ),claimed AS (
 UPDATE payment_action_jobs SET status='processing',checks=checks+1,claim_token=$1,lease_until=clock_timestamp()+interval '60 seconds',updated_at=clock_timestamp() WHERE id IN(SELECT id FROM candidate) RETURNING *
 ) SELECT j.id,j.kind,j.payment_id,COALESCE(j.refund_id,''),p.order_id,p.provider,COALESCE(p.transaction_id,''),o.price_minor,o.currency,j.reason,j.claim_token,j.attempts,j.checks,j.submitted FROM claimed j JOIN payment_intents p ON p.id=j.payment_id JOIN orders o ON o.id=p.order_id`, ID()).Scan(&j.ID, &j.Kind, &j.PaymentID, &j.RefundID, &j.OrderID, &j.Provider, &j.TransactionID, &j.Amount, &j.Currency, &j.Reason, &j.ClaimToken, &j.Attempts, &j.Checks, &j.Submitted)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return j, err
}

// The horizon check occurs before a network write, even for a recovered lease.
func (s *Store) PaymentActionWithinHorizon(ctx context.Context, id, token string) (bool, error) {
	var ok bool
	err := s.DB.QueryRow(ctx, `SELECT horizon_at>clock_timestamp() FROM payment_action_jobs WHERE id=$1 AND claim_token=$2 AND status='processing' AND lease_until>clock_timestamp()`, id, token).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrPaymentConflict
	}
	return ok, err
}
func (s *Store) MarkPaymentActionSubmitted(ctx context.Context, id, token string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var paymentID, kind string
	err = tx.QueryRow(ctx, `UPDATE payment_action_jobs SET submitted=true,updated_at=clock_timestamp() WHERE id=$1 AND claim_token=$2 AND status='processing' AND lease_until>clock_timestamp() AND horizon_at>clock_timestamp() RETURNING payment_id,kind`, id, token).Scan(&paymentID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPaymentConflict
	}
	if err != nil {
		return err
	}
	source := "gateway_close"
	if kind == "refund" {
		source = "refund"
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, "", source, "submitted", true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) FinishPaymentAction(ctx context.Context, id, token, outcome, providerRefundID string) error {
	switch outcome {
	case "closed", "paid", "succeeded", "pending", "not_found", "gateway_error", "verification_error", "storage_error", "conflict", "provider_unavailable", "unsupported", "failed", "manual_required":
	default:
		return ErrPaymentConflict
	}
	// Discover immutable ownership before locking in settlement order.
	var userID, paymentID, kind, refundID string
	err := s.DB.QueryRow(ctx, `SELECT o.user_id,j.payment_id,j.kind,COALESCE(j.refund_id,'') FROM payment_action_jobs j JOIN payment_intents p ON p.id=j.payment_id JOIN orders o ON o.id=p.order_id WHERE j.id=$1`, id).Scan(&userID, &paymentID, &kind, &refundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&locked); err != nil {
		return err
	}
	p, err := lockActionPayment(ctx, tx, paymentID)
	if err != nil {
		return err
	}
	var refund Refund
	if kind == "refund" {
		refund, err = scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM payment_refunds WHERE id=$1 FOR NO KEY UPDATE`, refundID))
		if err != nil {
			return err
		}
	}
	var attempts, checks int
	var within, submitted bool
	err = tx.QueryRow(ctx, `SELECT attempts,checks,horizon_at>clock_timestamp(),submitted FROM payment_action_jobs WHERE id=$1 AND claim_token=$2 AND status='processing' AND lease_until>clock_timestamp() FOR UPDATE`, id, token).Scan(&attempts, &checks, &within, &submitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPaymentConflict
	}
	if err != nil {
		return err
	}
	status, source := "waiting", "gateway_close"
	if kind == "close" {
		if p.Status == "paid" {
			outcome = "paid"
			status = "done"
		} else if outcome == "closed" {
			if p.OrderStatus != "closing" {
				return ErrPaymentConflict
			}
			if _, err = tx.Exec(ctx, `UPDATE orders SET status='closed',closed_at=clock_timestamp() WHERE id=$1`, p.OrderID); err != nil {
				return err
			}
			status = "done"
		} else if outcome == "succeeded" || outcome == "paid" {
			return ErrPaymentConflict
		}
	} else {
		source = "refund"
		if outcome == "closed" || outcome == "paid" {
			return ErrPaymentConflict
		}
		if outcome == "succeeded" {
			if p.Status != "paid" || p.OrderStatus != "paid" || providerRefundID == "" || len(providerRefundID) > 128 || strings.IndexFunc(providerRefundID, unicode.IsControl) >= 0 {
				return ErrPaymentConflict
			}
			if _, err = tx.Exec(ctx, `UPDATE payment_refunds SET status='succeeded',provider_refund_id=$2,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, refundID, providerRefundID); err != nil {
				return err
			}
			var entitlementID string
			err = tx.QueryRow(ctx, `UPDATE entitlement_periods SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE order_id=$1 RETURNING id`, p.OrderID).Scan(&entitlementID)
			if err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]string{"user_id": userID, "entitlement_id": entitlementID})
			if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(id,kind,aggregate_id,payload) VALUES($1,'entitlement.revoked',$2,$3) ON CONFLICT(kind,aggregate_id) DO NOTHING`, ID(), entitlementID, payload); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE orders SET status='refunded' WHERE id=$1`, p.OrderID); err != nil {
				return err
			}
			if refund.ApprovedBy == nil {
				return ErrPaymentConflict
			}
			if err = actionAudit(ctx, tx, *refund.ApprovedBy, "payment.refund_succeeded", refundID); err != nil {
				return err
			}
			status = "done"
		}
	}
	if status != "done" {
		if outcome == "pending" {
			attempts = 0
		} else {
			attempts++
		}
		if outcome == "failed" {
			status = "failed"
		} else if !within || attempts >= 8 || outcome == "manual_required" || outcome == "unsupported" {
			status = "manual_required"
		}
		if kind == "refund" {
			state := "queued"
			if submitted || outcome == "pending" {
				state = "processing"
			}
			if status == "manual_required" || status == "failed" {
				state = status
			}
			if _, err = tx.Exec(ctx, `UPDATE payment_refunds SET status=$2,updated_at=clock_timestamp() WHERE id=$1`, refundID, state); err != nil {
				return err
			}
		}
	}
	delay := 30
	exponent := checks
	if outcome != "pending" {
		exponent = attempts
	}
	for n := 1; n < exponent && delay < 1800; n++ {
		delay *= 2
	}
	if delay > 1800 {
		delay = 1800
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_action_jobs SET status=$2,attempts=$3,last_outcome=$4,next_attempt_at=clock_timestamp()+make_interval(secs=>$5),claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, id, status, attempts, outcome, delay); err != nil {
		return err
	}
	if err = recordPaymentActivityTx(ctx, tx, paymentID, p.Provider, source, outcome, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
