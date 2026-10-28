package payments

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"strings"
)

// Payment channels are opt-in. Configuration errors name the channel only;
// merchant secrets and filesystem contents never appear in errors.
func FromEnvironment() (*Registry, error) { return loadEnvironment(os.Getenv) }
func ActionsEnabledFromEnvironment() (bool, error) {
	return actionsEnabled(os.Getenv("PAYMENT_ACTIONS_ENABLED"))
}
func BillDownloadsEnabledFromEnvironment() (bool, error) {
	switch strings.TrimSpace(os.Getenv("BILL_DOWNLOADS_ENABLED")) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("BILL_DOWNLOADS_ENABLED must be true or false")
	}
}
func actionsEnabled(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("PAYMENT_ACTIONS_ENABLED must be true or false")
	}
}
func loadEnvironment(get func(string) string) (*Registry, error) {
	var providers []Provider
	seen := map[string]bool{}
	configured := strings.TrimSpace(get("PAYMENT_METHODS"))
	if configured == "" {
		return New()
	}
	for _, id := range strings.Split(configured, ",") {
		id = strings.TrimSpace(id)
		if seen[id] {
			return nil, errors.New("duplicate PAYMENT_METHODS entry")
		}
		seen[id] = true
		var provider Provider
		var err error
		switch id {
		case "alipay":
			private, e := readPrivate(get("ALIPAY_PRIVATE_KEY_FILE"))
			if e != nil {
				return nil, errors.New("invalid Alipay private key file")
			}
			public, e := readPublic(get("ALIPAY_PUBLIC_KEY_FILE"))
			if e != nil {
				return nil, errors.New("invalid Alipay public key file")
			}
			provider, err = NewAlipay(AlipayConfig{AppID: get("ALIPAY_APP_ID"), SellerID: get("ALIPAY_SELLER_ID"), Gateway: get("ALIPAY_GATEWAY"), PrivateKey: private, PublicKey: public})
		case "wechat":
			private, e := readPrivate(get("WECHAT_PRIVATE_KEY_FILE"))
			if e != nil {
				return nil, errors.New("invalid WeChat private key file")
			}
			public, e := readPublic(get("WECHAT_PLATFORM_PUBLIC_KEY_FILE"))
			if e != nil {
				return nil, errors.New("invalid WeChat platform key file")
			}
			provider, err = NewWechat(WechatConfig{AppID: get("WECHAT_APP_ID"), MchID: get("WECHAT_MCH_ID"), Serial: get("WECHAT_MERCHANT_SERIAL"), PlatformSerial: get("WECHAT_PLATFORM_SERIAL"), APIv3Key: get("WECHAT_API_V3_KEY"), Gateway: get("WECHAT_GATEWAY"), PrivateKey: private, PlatformKey: public})
		case "epay_alipay", "epay_wechat":
			kind := "alipay"
			if id == "epay_wechat" {
				kind = "wxpay"
			}
			provider, err = NewEpay(EpayConfig{PID: get("EPAY_PID"), Key: get("EPAY_KEY"), Gateway: get("EPAY_GATEWAY"), Type: kind})
		default:
			return nil, errors.New("unsupported PAYMENT_METHODS entry")
		}
		if err != nil {
			return nil, errors.New("invalid merchant configuration for " + id)
		}
		providers = append(providers, provider)
	}
	return New(providers...)
}
func readKey(path string) (*pem.Block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("key unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return nil, errors.New("key size invalid")
	}
	block, rest := pem.Decode(b)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid PEM key")
	}
	return block, nil
}
func readPrivate(path string) (*rsa.PrivateKey, error) {
	block, err := readKey(path)
	if err != nil {
		return nil, err
	}
	var key *rsa.PrivateKey
	if p, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
		key = p
	} else if p, e := x509.ParsePKCS8PrivateKey(block.Bytes); e == nil {
		key, _ = p.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("RSA private key required")
	}
	if err = key.Validate(); err != nil {
		return nil, errors.New("invalid RSA key")
	}
	return key, nil
}
func readPublic(path string) (*rsa.PublicKey, error) {
	block, err := readKey(path)
	if err != nil {
		return nil, err
	}
	var key *rsa.PublicKey
	if p, e := x509.ParsePKIXPublicKey(block.Bytes); e == nil {
		key, _ = p.(*rsa.PublicKey)
	} else if p, e := x509.ParsePKCS1PublicKey(block.Bytes); e == nil {
		key = p
	} else if p, e := x509.ParseCertificate(block.Bytes); e == nil {
		key, _ = p.PublicKey.(*rsa.PublicKey)
	}
	if key == nil || key.N.BitLen() < 2048 || key.E < 3 {
		return nil, errors.New("RSA public key required")
	}
	return key, nil
}
