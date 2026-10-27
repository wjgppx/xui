package payments

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMerchantConfigurationIsExplicitAndSecretsAreNotReported(t *testing.T) {
	values := map[string]string{"ALIPAY_PRIVATE_KEY_FILE": "missing-sensitive-key-path", "EPAY_KEY": "private-merchant-secret"}
	get := func(k string) string { return values[k] }
	r, err := loadEnvironment(get)
	if err != nil || len(r.Methods()) != 0 {
		t.Fatal("unconfigured payments must remain disabled")
	}
	for _, methods := range []string{"unknown", "alipay,alipay", ",", "alipay"} {
		values["PAYMENT_METHODS"] = methods
		_, err = loadEnvironment(get)
		if err == nil {
			t.Fatal("invalid merchant configuration accepted")
		}
		if strings.Contains(err.Error(), values["ALIPAY_PRIVATE_KEY_FILE"]) || strings.Contains(err.Error(), values["EPAY_KEY"]) {
			t.Fatal("configuration error exposed credentials")
		}
	}
	values["PAYMENT_METHODS"] = "epay_alipay,epay_wechat"
	values["EPAY_GATEWAY"] = "https://pay.example.test/"
	values["EPAY_PID"] = "1001"
	r, err = loadEnvironment(get)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Methods()) != 2 || r.Methods()[0].ID != "epay_alipay" || r.Methods()[1].ID != "epay_wechat" {
		t.Fatal("configured channels missing")
	}
	_, err = New(nil)
	if err == nil {
		t.Fatal("nil provider accepted")
	}
	p, _ := r.Get("epay_alipay")
	_, err = New(p, p)
	if err == nil {
		t.Fatal("duplicate provider accepted")
	}
}

func TestMerchantPEMFilesAndDirectChannelLoading(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath, publicPath := filepath.Join(dir, "merchant.pem"), filepath.Join(dir, "platform.pem")
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"PAYMENT_METHODS": "alipay,wechat", "ALIPAY_APP_ID": "2026000000000001", "ALIPAY_SELLER_ID": "2088000000000001", "ALIPAY_PRIVATE_KEY_FILE": privatePath, "ALIPAY_PUBLIC_KEY_FILE": publicPath,
		"WECHAT_APP_ID": "wx0123456789abcdef", "WECHAT_MCH_ID": "1900000001", "WECHAT_MERCHANT_SERIAL": "ABCDEF0123456789", "WECHAT_PLATFORM_SERIAL": "1234567890ABCDEF", "WECHAT_API_V3_KEY": "12345678901234567890123456789012", "WECHAT_PRIVATE_KEY_FILE": privatePath, "WECHAT_PLATFORM_PUBLIC_KEY_FILE": publicPath,
	}
	r, err := loadEnvironment(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Methods()) != 2 || r.Methods()[0].ID != "alipay" || r.Methods()[1].ID != "wechat" {
		t.Fatal("direct channels not loaded")
	}
	if err = os.WriteFile(publicPath, []byte("invalid key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readPublic(publicPath); err == nil {
		t.Fatal("invalid verification key accepted")
	}
	if err = os.WriteFile(privatePath, make([]byte, 65537), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readPrivate(privatePath); err == nil {
		t.Fatal("unbounded key file accepted")
	}
}
