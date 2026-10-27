package httpapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func TestSessionCreationRejectsPasswordCheckFromBeforeAccountChange(t *testing.T) {
	s := testutil.Database(t)
	id := testutil.User(t, s)
	// Login has already read and checked the old password. A reset or role change
	// commits before that request reaches session creation.
	old := store.User{ID: id, Role: "user", SecurityVersion: 0}
	if _, err := s.DB.Exec(context.Background(), `UPDATE users SET security_version=security_version+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	app := New(s, Config{Origin: "http://127.0.0.1:5173", PasswordCost: 4})
	response := httptest.NewRecorder()
	app.newSession(response, httptest.NewRequest("POST", "/api/v1/auth/login", nil), old)
	if response.Code != 401 || len(response.Result().Cookies()) != 0 {
		t.Fatal("stale password check created a session", response.Code)
	}
	var count int
	if err := s.DB.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id=$1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("stale login persisted a session")
	}
	// A fresh login uses the new generation and returns the role's permissions.
	old.SecurityVersion = 1
	response = httptest.NewRecorder()
	app.newSession(response, httptest.NewRequest("POST", "/api/v1/auth/login", nil), old)
	if response.Code != 200 || len(response.Result().Cookies()) != 1 {
		t.Fatal("current account state rejected", response.Code)
	}
	// Even a leftover row from an interrupted cleanup cannot authenticate if its
	// version predates the account's security generation.
	if _, err := s.DB.Exec(context.Background(), `UPDATE sessions SET security_version=0 WHERE user_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	req.AddCookie(response.Result().Cookies()[0])
	check := httptest.NewRecorder()
	app.Handler().ServeHTTP(check, req)
	if check.Code != 401 {
		t.Fatal("old-generation session authenticated", check.Code)
	}
}
