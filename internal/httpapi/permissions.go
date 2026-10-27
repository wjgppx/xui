package httpapi

import (
	"net/http"
	"strings"
)

var rolePermissions = map[string][]string{
	"admin":    {"overview.read", "orders.read", "plans.write", "nodes.read", "nodes.write", "access.read", "access.write", "events.read", "events.retry", "entitlements.revoke", "users.read", "users.roles", "payments.read", "payments.retry"},
	"support":  {"orders.read", "users.read"},
	"finance":  {"overview.read", "orders.read", "plans.write", "payments.read", "payments.retry"},
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
