// Package mailer delivers account mail over verified STARTTLS. It never makes a
// plaintext fallback and does not include transport responses in returned errors.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Config struct {
	Address, ServerName, Username, Password, From string
	// RootCAs is optional; nil uses system roots. Tests may install a private CA.
	RootCAs *x509.CertPool
}
type Message struct{ ID, To, Subject, Body string }
type Sender interface {
	Send(context.Context, Message) error
}
type smtpSender struct{ config Config }

var messageIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var ErrDelivery = errors.New("SMTP delivery failed")
var ErrMessage = errors.New("invalid mail message")

func bareMailbox(value string) bool {
	if value == "" || strings.ContainsAny(value, "\r\n") || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value
}
func headerText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func New(config Config) (Sender, error) {
	host, port, err := net.SplitHostPort(config.Address)
	number, numberErr := strconv.Atoi(port)
	if err != nil || numberErr != nil || number < 1 || number > 65535 || host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "\r\n/\\") || !bareMailbox(config.From) {
		return nil, errors.New("invalid SMTP address or sender")
	}
	if config.ServerName == "" {
		config.ServerName = host
	}
	if !headerText(config.ServerName) || strings.ContainsAny(config.ServerName, " /\\") || config.ServerName == "" {
		return nil, errors.New("invalid SMTP TLS server name")
	}
	if (config.Username == "") != (config.Password == "") {
		return nil, errors.New("SMTP username and password must be configured together")
	}
	return &smtpSender{config: config}, nil
}
func encodeMessage(from string, m Message) ([]byte, error) {
	if !bareMailbox(from) || !messageIDPattern.MatchString(m.ID) || !bareMailbox(m.To) || !headerText(m.Subject) || len(m.Subject) > 300 || !utf8.ValidString(m.Body) || len(m.Body) > 65536 {
		return nil, ErrMessage
	}
	var b bytes.Buffer
	domain := strings.ToLower(from[strings.LastIndex(from, "@")+1:])
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", from, m.To, mime.QEncoding.Encode("UTF-8", m.Subject), time.Now().UTC().Format(time.RFC1123Z), m.ID, domain)
	writer := quotedprintable.NewWriter(&b)
	if _, err := writer.Write([]byte(m.Body)); err != nil {
		return nil, ErrMessage
	}
	if err := writer.Close(); err != nil {
		return nil, ErrMessage
	}
	return b.Bytes(), nil
}
func (s *smtpSender) Send(ctx context.Context, m Message) error {
	data, err := encodeMessage(s.config.From, m)
	if err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(sendCtx, "tcp", s.config.Address)
	if err != nil {
		return ErrDelivery
	}
	defer connection.Close()
	deadline, _ := sendCtx.Deadline()
	if err = connection.SetDeadline(deadline); err != nil {
		return ErrDelivery
	}
	stop := context.AfterFunc(sendCtx, func() { connection.Close() })
	defer stop()
	client, err := smtp.NewClient(connection, s.config.ServerName)
	if err != nil {
		return ErrDelivery
	}
	defer client.Close()
	if available, _ := client.Extension("STARTTLS"); !available {
		return ErrDelivery
	}
	if err = client.StartTLS(&tls.Config{ServerName: s.config.ServerName, RootCAs: s.config.RootCAs, MinVersion: tls.VersionTLS12}); err != nil {
		return ErrDelivery
	}
	if s.config.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.ServerName)); err != nil {
			return ErrDelivery
		}
	}
	if err = client.Mail(s.config.From); err != nil {
		return ErrDelivery
	}
	if err = client.Rcpt(m.To); err != nil {
		return ErrDelivery
	}
	body, err := client.Data()
	if err != nil {
		return ErrDelivery
	}
	if _, err = body.Write(data); err != nil {
		body.Close()
		return ErrDelivery
	}
	if err = body.Close(); err != nil {
		return ErrDelivery
	}
	// SMTP DATA acceptance is the delivery boundary; a dropped QUIT reply does
	// not make us resend an already accepted message in this process.
	_ = client.Quit()
	return nil
}
