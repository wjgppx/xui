package store

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

var ErrPaymentProviderConflict = errors.New("order is pinned to another payment provider")
var ErrEmailUnverified = errors.New("verified email required")

type Payment struct {
	ID            string     `json:"id"`
	OrderID       string     `json:"order_id"`
	Provider      string     `json:"provider"`
	Status        string     `json:"status"`
	CheckoutKind  string     `json:"checkout_kind"`
	CheckoutURL   string     `json:"checkout_url"`
	TransactionID *string    `json:"transaction_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	PaidAt        *time.Time `json:"paid_at"`
}

const paymentColumns = `id,order_id,provider,status,checkout_kind,checkout_url,transaction_id,created_at,updated_at,paid_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.ID, &p.OrderID, &p.Provider, &p.Status, &p.CheckoutKind, &p.CheckoutURL, &p.TransactionID, &p.CreatedAt, &p.UpdatedAt, &p.PaidAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func paymentProvider(provider string) bool {
	return provider == "alipay" || provider == "wechat" || provider == "epay_alipay" || provider == "epay_wechat"
}

// PaymentOrder returns the immutable purchase snapshot, with an explicit owner
// predicate even when called before constructing an external checkout request.
func (s *Store) PaymentOrder(ctx context.Context, userID, orderID string) (Order, error) {
	var o Order
	err := s.DB.QueryRow(ctx, `SELECT id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes,status,created_at,paid_at FROM orders WHERE id=$1 AND user_id=$2`, orderID, userID).Scan(&o.ID, &o.UserID, &o.PlanID, &o.PlanName, &o.Price, &o.Currency, &o.Days, &o.Quota, &o.Status, &o.CreatedAt, &o.PaidAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

// CreatePayment commits a permanent merchant request identity before network
// I/O. Multiple processes therefore use the same out_trade_no. Channel switching
// requires a separate order; no second checkout can race a previous channel.
func (s *Store) CreatePayment(ctx context.Context, userID, orderID, provider string) (Payment, error) {
	if !paymentProvider(provider) {
		return Payment{}, ErrPaymentProviderConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Payment{}, err
	}
	defer tx.Rollback(ctx)
	var verified *time.Time
	err = tx.QueryRow(ctx, `SELECT email_verified_at FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, err
	}
	if s.RequireVerified && verified == nil {
		return Payment{}, ErrEmailUnverified
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 AND user_id=$2 FOR UPDATE`, orderID, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrNotFound
	}
	if err != nil {
		return Payment{}, err
	}
	if status != "pending" {
		return Payment{}, ErrPaymentConflict
	}
	p, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payment_intents WHERE order_id=$1`, orderID))
	if err == nil {
		if p.Provider != provider {
			return Payment{}, ErrPaymentProviderConflict
		}
		return p, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return Payment{}, err
	}
	p, err = scanPayment(tx.QueryRow(ctx, `INSERT INTO payment_intents(id,order_id,provider) VALUES($1,$2,$3) RETURNING `+paymentColumns, ID(), orderID, provider))
	if err != nil {
		return Payment{}, err
	}
	return p, tx.Commit(ctx)
}

func (s *Store) Payment(ctx context.Context, userID, orderID string) (Payment, error) {
	// SELECT a single table's columns to avoid accidental owner metadata exposure.
	return scanPayment(s.DB.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payment_intents WHERE order_id=$1 AND EXISTS(SELECT 1 FROM orders WHERE id=$1 AND user_id=$2)`, orderID, userID))
}

// SavePaymentCheckout stores only the first checkout. A callback may win this
// race before Create returns; saving the URL never downgrades a paid intent.
func (s *Store) SavePaymentCheckout(ctx context.Context, id, kind, link string) (Payment, error) {
	u, err := url.Parse(link)
	if err != nil || len(link) > 16384 || u.Host == "" || u.User != nil || strings.IndexFunc(link, unicode.IsControl) >= 0 || (kind != "redirect" && kind != "qr") || (u.Scheme != "https" && !(kind == "qr" && u.Scheme == "weixin")) {
		return Payment{}, ErrPaymentConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Payment{}, err
	}
	defer tx.Rollback(ctx)
	p, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payment_intents WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Payment{}, err
	}
	if p.Status != "paid" && p.CheckoutURL == "" {
		p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payment_intents SET status='ready',checkout_kind=$2,checkout_url=$3,updated_at=clock_timestamp() WHERE id=$1 RETURNING `+paymentColumns, id, kind, link))
		if err != nil {
			return Payment{}, err
		}
	}
	return p, tx.Commit(ctx)
}

// SettlePayment accepts only a provider-verified transaction. Intent status,
// payment evidence, purchased entitlement and invalidation event commit together.
func (s *Store) SettlePayment(ctx context.Context, paymentID, provider, transactionID string, amount int64, currency string) error {
	if !paymentProvider(provider) || transactionID == "" || len(transactionID) > 128 || strings.TrimSpace(transactionID) != transactionID || strings.IndexFunc(transactionID, unicode.IsControl) >= 0 {
		return ErrPaymentConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID, orderID string
	// Discover the immutable owner before acquiring the same user -> order lock
	// order used by legacy development settlement and renewal fulfillment.
	err = tx.QueryRow(ctx, `SELECT o.user_id,o.id FROM payment_intents p JOIN orders o ON o.id=p.order_id WHERE p.id=$1 AND p.provider=$2`, paymentID, provider).Scan(&userID, &orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// The helper holds user/order locks until this transaction commits. Its event
	// uniqueness also rejects a transaction replayed against a different order.
	if err = settleTx(ctx, tx, provider, transactionID, orderID, userID, amount, currency); err != nil {
		return err
	}
	p, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payment_intents WHERE id=$1 FOR UPDATE`, paymentID))
	if err != nil {
		return err
	}
	if p.Status == "paid" {
		if p.TransactionID == nil || *p.TransactionID != transactionID {
			return ErrPaymentConflict
		}
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_intents SET status='paid',transaction_id=$2,paid_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, paymentID, transactionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
