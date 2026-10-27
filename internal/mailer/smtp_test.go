package mailer

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type smtpFixture struct {
	address            string
	roots              *x509.CertPool
	messages           chan []byte
	accepts, plainAuth atomic.Int32
}

func newSMTPFixture(t *testing.T, advertiseTLS, dropQuit bool) *smtpFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp.test"}, DNSNames: []string{"smtp.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &smtpFixture{address: listener.Addr().String(), roots: roots, messages: make(chan []byte, 16)}
	var group sync.WaitGroup
	var connections sync.Map
	group.Add(1)
	go func() {
		defer group.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			fixture.accepts.Add(1)
			connections.Store(conn, true)
			group.Add(1)
			go func(raw net.Conn) {
				defer group.Done()
				defer raw.Close()
				defer connections.Delete(raw)
				conn := raw
				reader := bufio.NewReader(conn)
				secured := false
				reply := func(line string) bool { _, err := io.WriteString(conn, line+"\r\n"); return err == nil }
				if !reply("220 smtp.test ready") {
					return
				}
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					switch {
					case strings.HasPrefix(line, "EHLO"):
						if !reply("250-smtp.test") {
							return
						}
						if advertiseTLS && !secured {
							if !reply("250-STARTTLS") {
								return
							}
						}
						if !reply("250 AUTH PLAIN") {
							return
						}
					case line == "STARTTLS":
						if !advertiseTLS {
							reply("502 unavailable")
							continue
						}
						if !reply("220 begin TLS") {
							return
						}
						tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
						if err = tlsConn.Handshake(); err != nil {
							return
						}
						conn = tlsConn
						reader = bufio.NewReader(conn)
						secured = true
					case strings.HasPrefix(line, "AUTH PLAIN "):
						if !secured {
							fixture.plainAuth.Add(1)
						}
						credentials, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
						if string(credentials) != "\x00smtp-user\x00smtp-password" {
							reply("535 rejected")
							continue
						}
						if !reply("235 authenticated") {
							return
						}
					case strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:"):
						if !secured {
							reply("530 encryption required")
							continue
						}
						if !reply("250 accepted") {
							return
						}
					case line == "DATA":
						if !reply("354 start mail") {
							return
						}
						data, err := textproto.NewReader(reader).ReadDotBytes()
						if err != nil {
							return
						}
						fixture.messages <- data
						if !reply("250 stored") {
							return
						}
						if dropQuit {
							return
						}
					case line == "QUIT":
						reply("221 closing")
						return
					default:
						if !reply("500 command rejected") {
							return
						}
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Range(func(key, value any) bool { key.(net.Conn).Close(); return true })
		group.Wait()
	})
	return fixture
}
func testMessage() Message {
	return Message{ID: strings.Repeat("1", 32), To: "user@example.test", Subject: "验证你的邮箱", Body: "打开链接完成验证\nhttps://control.example.test/#account-action=verify&token=opaque-token"}
}
func TestSMTPVerifiedSTARTTLSMessageEncodingAndAcceptedDataBoundary(t *testing.T) {
	fixture := newSMTPFixture(t, true, true)
	sender, err := New(Config{Address: fixture.address, ServerName: "smtp.test", From: "noreply@example.test", Username: "smtp-user", Password: "smtp-password", RootCAs: fixture.roots})
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-fixture.messages:
		message, err := mail.ReadMessage(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
		if err != nil || subject != testMessage().Subject {
			t.Fatal("subject encoding corrupted")
		}
		body, err := io.ReadAll(quotedprintable.NewReader(message.Body))
		// SMTP DATA ends on a complete CRLF-terminated line even when the
		// original text does not include a final newline.
		if err != nil || strings.TrimSuffix(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") != testMessage().Body {
			t.Fatal("mail body encoding corrupted")
		}
		if message.Header.Get("Message-ID") != "<"+testMessage().ID+"@xui.local>" {
			t.Fatal("unstable message identity")
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP mail not received")
	}
	if fixture.plainAuth.Load() != 0 {
		t.Fatal("credentials sent without TLS")
	}
}
func TestSMTPRejectsMissingSTARTTLSBadCertificateAndHeaderInjection(t *testing.T) {
	for _, tlsAvailable := range []bool{false, true} {
		fixture := newSMTPFixture(t, tlsAvailable, false)
		sender, err := New(Config{Address: fixture.address, ServerName: "smtp.test", From: "noreply@example.test", Username: "smtp-user", Password: "smtp-password"})
		if err != nil {
			t.Fatal(err)
		}
		if err = sender.Send(context.Background(), testMessage()); err != ErrDelivery {
			t.Fatal("unsafe SMTP accepted", tlsAvailable, err)
		}
		if fixture.plainAuth.Load() != 0 {
			t.Fatal("credential disclosed before TLS")
		}
	}
	fixture := newSMTPFixture(t, true, false)
	if _, err := New(Config{Address: fixture.address, From: "noreply@example.test\r\nBcc: victim@example.test"}); err == nil {
		t.Fatal("sender header injection accepted")
	}
	sender, err := New(Config{Address: fixture.address, ServerName: "smtp.test", From: "noreply@example.test", RootCAs: fixture.roots})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Message{{ID: testMessage().ID, To: "user@example.test\r\nBcc: victim@example.test", Subject: "safe"}, {ID: testMessage().ID, To: "user@example.test", Subject: "safe\r\nBcc: victim@example.test"}, {ID: "bad\r\nHeader", To: "user@example.test", Subject: "safe"}} {
		if err = sender.Send(context.Background(), bad); err != ErrMessage {
			t.Fatal("header injection accepted")
		}
	}
	if fixture.accepts.Load() != 0 {
		t.Fatal("invalid mail reached SMTP server")
	}
}
func TestSMTPCancellationInterruptsStalledGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			io.Copy(io.Discard, conn)
		}
	}()
	sender, err := New(Config{Address: listener.Addr().String(), From: "noreply@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err = sender.Send(ctx, testMessage()); err != ErrDelivery {
		t.Fatal("stalled SMTP not interrupted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("SMTP cancellation too slow")
	}
	<-done
}
