package mailer

import (
	"context"
	"crypto/tls"
	"net"
	"net/smtp"
	"time"
)

type CheckResult struct {
	TLSVerified      bool `json:"tls_verified"`
	Authenticated    bool `json:"authenticated"`
	MessageSubmitted bool `json:"message_submitted"`
}

// Check verifies the actual SMTP greeting, STARTTLS certificate and optional
// authentication, then quits before MAIL, RCPT or DATA. It sends no message.
func Check(ctx context.Context, config Config) (CheckResult, error) {
	sender, err := New(config)
	if err != nil {
		return CheckResult{}, err
	}
	config = sender.(*smtpSender).config
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(operation, "tcp", config.Address)
	if err != nil {
		return CheckResult{}, ErrDelivery
	}
	defer connection.Close()
	deadline, _ := operation.Deadline()
	if err = connection.SetDeadline(deadline); err != nil {
		return CheckResult{}, ErrDelivery
	}
	stop := context.AfterFunc(operation, func() { connection.Close() })
	defer stop()
	client, err := smtp.NewClient(connection, config.ServerName)
	if err != nil {
		return CheckResult{}, ErrDelivery
	}
	defer client.Close()
	if available, _ := client.Extension("STARTTLS"); !available {
		return CheckResult{}, ErrDelivery
	}
	if err = client.StartTLS(&tls.Config{ServerName: config.ServerName, RootCAs: config.RootCAs, MinVersion: tls.VersionTLS12}); err != nil {
		return CheckResult{}, ErrDelivery
	}
	if config.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", config.Username, config.Password, config.ServerName)); err != nil {
			return CheckResult{}, ErrDelivery
		}
	}
	_ = client.Quit()
	return CheckResult{TLSVerified: true, Authenticated: config.Username != ""}, nil
}
