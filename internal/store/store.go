package store

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

//go:embed nodes.sql
var nodeSchema string

//go:embed proxy.sql
var proxySchema string

//go:embed usage.sql
var usageSchema string

//go:embed subscriptions.sql
var subscriptionSchema string

//go:embed groups.sql
var groupSchema string

//go:embed security.sql
var securitySchema string

//go:embed payments.sql
var paymentSchema string

//go:embed payment_lifecycle.sql
var paymentLifecycleSchema string

//go:embed payment_actions.sql
var paymentActionsSchema string

//go:embed merchant_statements.sql
var merchantStatementsSchema string

//go:embed bill_downloads.sql
var billDownloadsSchema string

//go:embed bill_schedules.sql
var billSchedulesSchema string

//go:embed statement_financials.sql
var statementFinancialsSchema string

var ErrNotFound = errors.New("not found")
var ErrPaymentConflict = errors.New("payment conflicts with order")

type Store struct {
	DB *pgxpool.Pool
	// Immutable startup policy; production excludes unverified users from grants.
	RequireVerified bool
}
type User struct {
	EmailVerifiedAt *time.Time `json:"email_verified_at"`
	SecurityVersion int64      `json:"-"`
	Permissions     []string   `json:"permissions"`
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	Role            string     `json:"role"`
}
type Plan struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Price       int64  `json:"price_minor"`
	Currency    string `json:"currency"`
	Days        int    `json:"duration_days"`
	Quota       int64  `json:"quota_bytes"`
}
type Order struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	PlanID    string     `json:"plan_id"`
	PlanName  string     `json:"plan_name"`
	Price     int64      `json:"price_minor"`
	Currency  string     `json:"currency"`
	Days      int        `json:"duration_days"`
	Quota     int64      `json:"quota_bytes"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	PaidAt    *time.Time `json:"paid_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	ClosedAt  *time.Time `json:"closed_at"`
}
type Entitlement struct {
	ID        string     `json:"id"`
	OrderID   string     `json:"order_id"`
	PlanName  string     `json:"plan_name"`
	StartsAt  time.Time  `json:"starts_at"`
	EndsAt    time.Time  `json:"ends_at"`
	Quota     int64      `json:"quota_bytes"`
	Used      int64      `json:"used_bytes"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(738912401)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for i, migration := range []string{schema, nodeSchema, proxySchema, usageSchema, subscriptionSchema, groupSchema, securitySchema, paymentSchema, paymentLifecycleSchema, paymentActionsSchema, merchantStatementsSchema, billDownloadsSchema, billSchedulesSchema, statementFinancialsSchema} {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, i+1).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err = tx.Exec(ctx, migration); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, i+1); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) Seed(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO plans(id,name,description,price_minor,currency,duration_days,quota_bytes) VALUES
 ('explore','轻量探索','适合个人日常使用，轻松开始。',1900,'CNY',30,107374182400),
 ('everyday','日常进阶','为多设备与持续工作准备的充裕空间。',3900,'CNY',30,322122547200),
 ('studio','工作室','面向更高用量的工作与协作。',7900,'CNY',30,1073741824000)
 ON CONFLICT(id) DO NOTHING`)
	return err
}
func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,description,price_minor,currency,duration_days,quota_bytes FROM plans WHERE active ORDER BY price_minor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		var p Plan
		if err = rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Currency, &p.Days, &p.Quota); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) CreateOrder(ctx context.Context, userID, planID string) (Order, error) {
	var o Order
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return o, err
	}
	defer tx.Rollback(ctx)
	var plan Plan
	err = tx.QueryRow(ctx, `SELECT id,name,price_minor,currency,duration_days,quota_bytes FROM plans WHERE id=$1 AND active FOR SHARE`, planID).Scan(&plan.ID, &plan.Name, &plan.Price, &plan.Currency, &plan.Days, &plan.Quota)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO orders(id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 RETURNING id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes,status,created_at,paid_at,expires_at,closed_at`, ID(), userID, plan.ID, plan.Name, plan.Price, plan.Currency, plan.Days, plan.Quota).Scan(&o.ID, &o.UserID, &o.PlanID, &o.PlanName, &o.Price, &o.Currency, &o.Days, &o.Quota, &o.Status, &o.CreatedAt, &o.PaidAt, &o.ExpiresAt, &o.ClosedAt)
	if err != nil {
		return o, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO order_node_groups(order_id,group_id) SELECT $1,group_id FROM plan_node_groups WHERE plan_id=$2`, o.ID, plan.ID); err != nil {
		return o, err
	}
	return o, tx.Commit(ctx)
}
func (s *Store) Orders(ctx context.Context, userID string) ([]Order, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes,CASE WHEN status='pending' AND expires_at<=clock_timestamp() THEN 'expired' ELSE status END,created_at,paid_at,expires_at,closed_at FROM orders WHERE ($1='' OR user_id=$1) ORDER BY created_at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		var o Order
		if err = rows.Scan(&o.ID, &o.UserID, &o.PlanID, &o.PlanName, &o.Price, &o.Currency, &o.Days, &o.Quota, &o.Status, &o.CreatedAt, &o.PaidAt, &o.ExpiresAt, &o.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Store) Entitlements(ctx context.Context, userID string) ([]Entitlement, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,order_id,plan_name,starts_at,ends_at,quota_bytes,used_bytes,revoked_at FROM entitlement_periods WHERE user_id=$1 ORDER BY starts_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entitlement{}
	for rows.Next() {
		var e Entitlement
		if err = rows.Scan(&e.ID, &e.OrderID, &e.PlanName, &e.StartsAt, &e.EndsAt, &e.Quota, &e.Used, &e.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Settle is called only after provider verification. The development adapter gets
// amount/currency from the stored order, never from browser-supplied payment data.
// The user lock serializes different renewal orders as well as duplicate events.
func (s *Store) Settle(ctx context.Context, provider, eventID, orderID, userID string, amount int64, currency string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = settleTx(ctx, tx, provider, eventID, orderID, userID, amount, currency, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// settleTx preserves one transaction for verified payment intent updates and
// business fulfillment. User then order locks serialize all renewal paths.
func settleTx(ctx context.Context, tx pgx.Tx, provider, eventID, orderID, userID string, amount int64, currency string, allowRemoteClosed bool) error {
	var err error
	var locked string
	// This lock serializes payments but must remain compatible with the KEY
	// SHARE locks used by grant foreign keys. A stronger user-row lock can form
	// a cycle with group edits and node snapshot creation.
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&locked); err != nil {
		return ErrNotFound
	}
	var o Order
	err = tx.QueryRow(ctx, `SELECT id,user_id,plan_name,price_minor,currency,duration_days,quota_bytes,status FROM orders WHERE id=$1 AND user_id=$2 FOR NO KEY UPDATE`, orderID, userID).Scan(&o.ID, &o.UserID, &o.PlanName, &o.Price, &o.Currency, &o.Days, &o.Quota, &o.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if amount != o.Price || currency != o.Currency {
		return ErrPaymentConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO payment_events(provider,event_id,order_id,amount_minor,currency) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, provider, eventID, orderID, amount, currency)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var match bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_events WHERE provider=$1 AND event_id=$2 AND order_id=$3 AND amount_minor=$4 AND currency=$5)`, provider, eventID, orderID, amount, currency).Scan(&match)
		if err != nil {
			return err
		}
		if !match {
			return ErrPaymentConflict
		}
		return nil
	}
	if o.Status != "pending" && o.Status != "expired" && !(allowRemoteClosed && (o.Status == "closing" || o.Status == "closed")) {
		return ErrPaymentConflict
	}
	var start time.Time
	if err = tx.QueryRow(ctx, `SELECT GREATEST(now(),COALESCE(MAX(ends_at),now())) FROM entitlement_periods WHERE user_id=$1`, userID).Scan(&start); err != nil {
		return err
	}
	entitlementID := ID()
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_periods(id,user_id,order_id,plan_name,starts_at,ends_at,quota_bytes) VALUES($1,$2,$3,$4,$5,$6,$7)`, entitlementID, userID, orderID, o.PlanName, start, start.Add(time.Duration(o.Days)*24*time.Hour), o.Quota); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_node_groups(entitlement_period_id,group_id) SELECT $1,group_id FROM order_node_groups WHERE order_id=$2`, entitlementID, orderID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET status='paid',paid_at=now(),closed_at=NULL WHERE id=$1`, orderID); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"user_id": userID, "entitlement_id": entitlementID})
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(id,kind,aggregate_id,payload) VALUES($1,'entitlement.granted',$2,$3)`, ID(), orderID, payload); err != nil {
		return err
	}
	return nil
}
