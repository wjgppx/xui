package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wjgppx/xui/internal/billfetch"
	"github.com/wjgppx/xui/internal/clientip"
	"github.com/wjgppx/xui/internal/controlconfig"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/mailer"
	"github.com/wjgppx/xui/internal/paymentactions"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/reconcile"
	"github.com/wjgppx/xui/internal/store"
)

var releaseID = "development"

func main() {
	admin := flag.String("promote-admin", "", "Promote an existing registered email, then exit")
	demote := flag.String("demote-admin", "", "Return an existing administrator to the user role, then exit")
	setRole := flag.String("set-role", "", "Change the role of an existing registered email, then exit")
	roleName := flag.String("role", "", "Role for --set-role: user, admin, support, finance, operator")
	resetMFA := flag.String("reset-staff-mfa", "", "Host-console recovery: remove an existing staff account's MFA and revoke all sessions, then exit")
	flag.Parse()
	operations := 0
	for _, v := range []string{*admin, *demote, *setRole, *resetMFA} {
		if v != "" {
			operations++
		}
	}
	if operations > 1 || (*setRole != "" && !store.ValidRole(*roleName)) || (*roleName != "" && *setRole == "") {
		slog.Error("choose one role operation and a valid role for --set-role")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		slog.Error("database configuration invalid")
		os.Exit(1)
	}
	defer db.Close()
	s := &store.Store{DB: db}
	if err = s.Migrate(ctx); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
	if operations == 1 {
		email, role := *admin, "admin"
		if *resetMFA != "" {
			email = *resetMFA
		}
		if *demote != "" {
			email, role = *demote, "user"
		}
		if *setRole != "" {
			email, role = *setRole, *roleName
		}
		var userID string
		if err = db.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, strings.ToLower(strings.TrimSpace(email))).Scan(&userID); err != nil {
			slog.Error("existing account required")
			os.Exit(1)
		}
		if *resetMFA != "" {
			if err = s.ResetStaffMFA(ctx, userID, userID); err != nil {
				slog.Error("staff second-factor recovery failed")
				os.Exit(1)
			}
			slog.Info("host-console staff second-factor recovery completed; all sessions revoked")
			return
		}
		if err = s.SetUserRole(ctx, userID, userID, role, true); err != nil {
			slog.Error("role update failed")
			os.Exit(1)
		}
		slog.Info("account role operation completed", "role", role)
		return
	}
	mode := os.Getenv("APP_MODE")
	if mode != "development" && mode != "production" {
		slog.Error("APP_MODE must be development or production")
		os.Exit(1)
	}
	trustedProxy, err := clientip.Parse(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		slog.Error("trusted proxy configuration invalid")
		os.Exit(1)
	}
	requireStaffMFA, mfaKey, err := controlconfig.StaffMFA(mode == "production", os.Getenv("REQUIRE_STAFF_MFA"), os.Getenv("MFA_KEY_FILE"))
	if err != nil {
		slog.Error("staff second-factor configuration invalid", "error", err)
		os.Exit(1)
	}
	origin := os.Getenv("PUBLIC_ORIGIN")
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" || u.Path != "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		slog.Error("PUBLIC_ORIGIN must be a single HTTP(S) origin")
		os.Exit(1)
	}
	if mode == "production" && u.Scheme != "https" {
		slog.Error("production requires HTTPS PUBLIC_ORIGIN")
		os.Exit(1)
	}
	if mode == "development" {
		if err = s.Seed(ctx); err != nil {
			slog.Error("seed failed", "error", err)
			os.Exit(1)
		}
	}
	s.RequireVerified = mode == "production"
	var sender mailer.Sender
	if mode == "production" {
		sender, err = mailer.New(mailer.Config{Address: os.Getenv("SMTP_ADDR"), ServerName: os.Getenv("SMTP_TLS_SERVER_NAME"), Username: os.Getenv("SMTP_USER"), Password: os.Getenv("SMTP_PASSWORD"), From: os.Getenv("SMTP_FROM")})
		if err != nil {
			slog.Error("production requires valid SMTP_ADDR and SMTP_FROM, with paired optional SMTP_USER and SMTP_PASSWORD")
			os.Exit(1)
		}
	}
	registry, err := payments.FromEnvironment()
	if err != nil {
		slog.Error("payment configuration invalid", "error", err)
		os.Exit(1)
	}
	actionsEnabled, err := payments.ActionsEnabledFromEnvironment()
	if err != nil {
		slog.Error("payment action configuration invalid")
		os.Exit(1)
	}
	billDownloadsEnabled, err := payments.BillDownloadsEnabledFromEnvironment()
	if err != nil {
		slog.Error("bill download configuration invalid")
		os.Exit(1)
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); s.RunOutbox(ctx) }()
	defer func() { stop(); <-workerDone }()
	if sender != nil {
		mailDone := make(chan struct{})
		go func() { defer close(mailDone); mailer.Run(ctx, s, sender) }()
		defer func() { stop(); <-mailDone }()
	}
	reconcileDone := make(chan struct{})
	go func() { defer close(reconcileDone); reconcile.Run(ctx, s, registry, origin) }()
	defer func() { stop(); <-reconcileDone }()
	if actionsEnabled {
		actionDone := make(chan struct{})
		go func() { defer close(actionDone); paymentactions.Run(ctx, s, registry, origin) }()
		defer func() { stop(); <-actionDone }()
	}
	if billDownloadsEnabled {
		scheduleDone := make(chan struct{})
		go func() { defer close(scheduleDone); billfetch.RunSchedules(ctx, s, registry) }()
		defer func() { stop(); <-scheduleDone }()
		billDone := make(chan struct{})
		go func() { defer close(billDone); billfetch.Run(ctx, s, registry) }()
		defer func() { stop(); <-billDone }()
	}
	app := httpapi.New(s, httpapi.Config{Origin: origin, Development: mode == "development", SecureCookies: u.Scheme == "https", RequireVerified: s.RequireVerified, Payments: registry, PaymentActionsEnabled: actionsEnabled, BillDownloadsEnabled: billDownloadsEnabled, TrustedProxy: trustedProxy, MFAKey: mfaKey, RequireStaffMFA: requireStaffMFA, ReleaseID: releaseID})
	server := &http.Server{Addr: addr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	shutdownDone := shutdownOnCancel(ctx, server, 10*time.Second)
	slog.Info("control API started", "address", addr, "mode", mode)
	err = server.ListenAndServe()
	stop()
	// Shutdown closes the listener before draining handlers. Keep the database
	// and process alive until the drain finishes (or its deadline forces Close).
	<-shutdownDone
	if err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
