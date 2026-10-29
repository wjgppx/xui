package httpapi

import (
	"net/http"
	"strings"
)

var rolePermissions = map[string][]string{
	"admin":    {"overview.read", "orders.read", "plans.write", "nodes.read", "nodes.write", "access.read", "access.write", "events.read", "events.retry", "entitlements.revoke", "users.read", "users.roles", "payments.read", "payments.retry", "payments.close", "refunds.read", "refunds.request", "refunds.approve", "statements.read", "statements.import", "statements.reconcile", "statements.review", "statements.download", "statements.schedule", "funds.read", "funds.import", "funds.reconcile"},
	"support":  {"orders.read", "users.read"},
	"finance":  {"overview.read", "orders.read", "plans.write", "payments.read", "payments.retry", "payments.close", "refunds.read", "refunds.request", "statements.read", "statements.import", "statements.reconcile", "statements.review", "statements.download", "statements.schedule", "funds.read", "funds.import", "funds.reconcile"},
	"operator": {"nodes.read", "nodes.write", "access.read", "access.write", "events.read", "events.retry"},
	"user":     {},
}

func permissions(role string) []string { return append([]string{}, rolePermissions[role]...) }
func permitted(role, permission string) bool {
	for _, p := range rolePermissions[role] {
		if p == permission {
			return true
		}
	}
	return false
}
func requiredPermission(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/api/v1/admin/fund-ledgers":
		if r.Method == "GET" {
			return "funds.read"
		}
		if r.Method == "POST" {
			return "funds.import"
		}
		return "unmapped.deny"
	case p == "/api/v1/admin/fund-reconciliations":
		if r.Method == "GET" {
			return "funds.read"
		}
		if r.Method == "POST" {
			return "funds.reconcile"
		}
		return "unmapped.deny"
	case strings.HasPrefix(p, "/api/v1/admin/fund-ledgers/"), strings.HasPrefix(p, "/api/v1/admin/fund-reconciliations/"):
		if r.Method == "GET" {
			return "funds.read"
		}
		return "unmapped.deny"
	case p == "/api/v1/admin/bill-schedules":
		if r.Method == "GET" {
			return "statements.read"
		}
		if r.Method == "PUT" {
			return "statements.schedule"
		}
		return "unmapped.deny"
	case p == "/api/v1/admin/bill-downloads":
		if r.Method == "GET" {
			return "statements.read"
		}
		if r.Method == "POST" {
			return "statements.download"
		}
		return "unmapped.deny"
	case p == "/api/v1/admin/statements/native" && r.Method == "POST":
		return "statements.import"
	case p == "/api/v1/admin/statements":
		if r.Method == "GET" {
			return "statements.read"
		}
		return "statements.import"
	case strings.HasPrefix(p, "/api/v1/admin/statements/"):
		if r.Method == "POST" && strings.HasSuffix(p, "/reconcile") {
			return "statements.reconcile"
		}
		if r.Method == "GET" {
			return "statements.read"
		}
		return "unmapped.deny"
	case strings.HasPrefix(p, "/api/v1/admin/statement-findings/") && strings.HasSuffix(p, "/reviews"):
		if r.Method == "POST" {
			return "statements.review"
		}
		if r.Method == "GET" {
			return "statements.read"
		}
		return "unmapped.deny"
	case p == "/api/v1/admin/refunds":
		return "refunds.read"
	case strings.HasPrefix(p, "/api/v1/admin/refunds/") && strings.HasSuffix(p, "/approve"):
		return "refunds.approve"
	case strings.HasPrefix(p, "/api/v1/admin/payments/") && strings.HasSuffix(p, "/refund"):
		return "refunds.request"
	case strings.HasPrefix(p, "/api/v1/admin/payments/") && strings.HasSuffix(p, "/close"):
		return "payments.close"
	case p == "/api/v1/admin/payments" || p == "/api/v1/admin/payment-activity":
		return "payments.read"
	case strings.HasPrefix(p, "/api/v1/admin/payments/"):
		if r.Method == "GET" {
			return "payments.read"
		}
		return "payments.retry"
	case p == "/api/v1/admin/overview":
		return "overview.read"
	case p == "/api/v1/admin/orders":
		return "orders.read"
	case p == "/api/v1/admin/plans":
		return "plans.write"
	case p == "/api/v1/admin/access":
		return "access.read"
	case strings.HasPrefix(p, "/api/v1/admin/nodes"):
		if r.Method == "GET" {
			return "nodes.read"
		}
		return "nodes.write"
	case strings.HasPrefix(p, "/api/v1/admin/node-groups"), strings.HasPrefix(p, "/api/v1/admin/plans/"):
		return "access.write"
	case p == "/api/v1/admin/events":
		return "events.read"
	case strings.HasPrefix(p, "/api/v1/admin/events/"):
		return "events.retry"
	case strings.HasPrefix(p, "/api/v1/admin/entitlements/"):
		return "entitlements.revoke"
	case p == "/api/v1/admin/users":
		return "users.read"
	case strings.HasPrefix(p, "/api/v1/admin/users/"):
		return "users.roles"
	default:
		return "unmapped.deny"
	}
}
func verificationProtected(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/api/v1/admin/") || p == "/api/v1/me/subscription/rotate" || p == "/api/v1/me/proxy-credentials/rotate" || (r.Method == "POST" && (p == "/api/v1/orders" || strings.HasPrefix(p, "/api/v1/dev/orders/")))
}
