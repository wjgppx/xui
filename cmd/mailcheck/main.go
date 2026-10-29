// mailcheck defaults to transport/authentication checks without submitting mail.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"time"

	"github.com/wjgppx/xui/internal/mailer"
)

func main() {
	send := flag.Bool("send-test", false, "Explicitly submit one test message to --to; default only verifies TLS/authentication")
	to := flag.String("to", "", "Explicit recipient for --send-test; never defaults to an account address")
	flag.Parse()
	if flag.NArg() != 0 || (*send != (*to != "")) {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": "use --send-test and --to together, or omit both for a non-sending check"})
		os.Exit(2)
	}
	config := mailer.Config{Address: os.Getenv("SMTP_ADDR"), ServerName: os.Getenv("SMTP_TLS_SERVER_NAME"), Username: os.Getenv("SMTP_USER"), Password: os.Getenv("SMTP_PASSWORD"), From: os.Getenv("SMTP_FROM")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := mailer.Check(ctx, config)
	if err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": "SMTP configuration, verified TLS or authentication check failed"})
		os.Exit(1)
	}
	if *send {
		var random [16]byte
		if _, err = rand.Read(random[:]); err == nil {
			var sender mailer.Sender
			sender, err = mailer.New(config)
			if err == nil {
				err = sender.Send(ctx, mailer.Message{ID: hex.EncodeToString(random[:]), To: *to, Subject: "XUI 邮件投递验收", Body: "这是操作人员明确发起的 XUI 邮件投递测试。SMTP 接收成功不等于收件箱到达，请在目标邮箱中检查此邮件及其 SPF、DKIM、DMARC 结果。"})
			}
		}
		if err != nil {
			_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": "SMTP test message was not confirmed accepted"})
			os.Exit(1)
		}
		result.MessageSubmitted = true
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
