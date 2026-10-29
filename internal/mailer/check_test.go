package mailer

import (
	"context"
	"testing"
)

func TestSMTPPreflightAuthenticatesWithoutSubmittingAnyMessage(t *testing.T) {
	fixture := newSMTPFixture(t, true, false)
	config := Config{Address: fixture.address, ServerName: "smtp.test", RootCAs: fixture.roots, From: "noreply@example.test", Username: "smtp-user", Password: "smtp-password"}
	result, err := Check(context.Background(), config)
	if err != nil || !result.TLSVerified || !result.Authenticated || result.MessageSubmitted || len(fixture.messages) != 0 || fixture.plainAuth.Load() != 0 {
		t.Fatal("SMTP preflight submitted mail or skipped verified transport", result, err)
	}
	config.Password = "wrong-password"
	if _, err = Check(context.Background(), config); err == nil || len(fixture.messages) != 0 {
		t.Fatal("failed authentication accepted or sent a message")
	}
	config.Password = "smtp-password"
	config.RootCAs = nil
	if _, err = Check(context.Background(), config); err == nil {
		t.Fatal("untrusted SMTP certificate accepted")
	}
}

func TestSMTPPreflightNeverFallsBackToPlaintextAndHonorsCancellation(t *testing.T) {
	fixture := newSMTPFixture(t, false, false)
	config := Config{Address: fixture.address, ServerName: "smtp.test", RootCAs: fixture.roots, From: "noreply@example.test"}
	if _, err := Check(context.Background(), config); err == nil || len(fixture.messages) != 0 {
		t.Fatal("plaintext SMTP accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Check(ctx, config); err == nil {
		t.Fatal("canceled probe continued")
	}
}
