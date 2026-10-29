package httpapi

import (
	"context"
	"net/http"

	"github.com/wjgppx/xui/internal/clientip"
)

type clientAddressKey struct{}

func clientAddress(r *http.Request) string {
	if value, ok := r.Context().Value(clientAddressKey{}).(string); ok {
		return value
	}
	address, _ := (*clientip.Policy)(nil).Resolve(r.RemoteAddr, nil)
	return address
}

func (s *Server) addressRequest(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	address, err := s.Config.TrustedProxy.Resolve(r.RemoteAddr, r.Header)
	if err != nil {
		fail(w, http.StatusBadRequest, "可信代理转发链格式不正确")
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), clientAddressKey{}, address)), true
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	var schema int
	if err := s.Store.DB.QueryRow(r.Context(), `SELECT COALESCE(max(version),0) FROM schema_migrations`).Scan(&schema); err != nil {
		fail(w, http.StatusServiceUnavailable, "服务尚未就绪")
		return
	}
	release := s.Config.ReleaseID
	if release == "" {
		release = "development"
	}
	reply(w, http.StatusOK, map[string]any{"status": "ok", "release": release, "schema_version": schema})
}
