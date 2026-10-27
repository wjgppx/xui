package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/wjgppx/xui/internal/store"
)

func (s *Server) staffUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	rows, err := s.Store.DB.Query(r.Context(), `SELECT id,email,role,email_verified_at,created_at FROM users ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID       string     `json:"id"`
		Email    string     `json:"email"`
		Role     string     `json:"role"`
		Verified *time.Time `json:"email_verified_at"`
		Created  time.Time  `json:"created_at"`
	}
	result := []item{}
	for rows.Next() {
		var u item
		if err = rows.Scan(&u.ID, &u.Email, &u.Role, &u.Verified, &u.Created); err != nil {
			serverError(w, err)
			return
		}
		result = append(result, u)
	}
	if err = rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, result)
}
func (s *Server) setStaffRole(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !store.ValidRole(body.Role) {
		fail(w, 400, "后台角色不正确")
		return
	}
	err := s.Store.SetUserRole(r.Context(), u.ID, r.PathValue("id"), body.Role, false)
	if errors.Is(err, store.ErrRoleForbidden) {
		fail(w, 403, "管理员权限已变化，请重新登录")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		fail(w, 404, "用户不存在")
		return
	}
	if errors.Is(err, store.ErrRoleConflict) {
		fail(w, 409, "不能取消自己的管理员权限或移除最后一位管理员")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
