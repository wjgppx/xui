package payments

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// WechatConfig uses one explicitly pinned WeChat Pay platform key. PlatformSerial
// accepts a certificate serial or the PUB_KEY_ID_* identifier supplied by WeChat.
// Key rotation is an operator configuration change; unsigned discovery is never
// used to replace the key that authenticates a response or notification.
type WechatConfig struct {
	AppID, MchID, Serial, PlatformSerial, APIv3Key, Gateway string
	PrivateKey                                              *rsa.PrivateKey
	PlatformKey                                             *rsa.PublicKey
	HTTPClient                                              *http.Client
	Now                                                     func() time.Time
}

type wechatProvider struct {
	config WechatConfig
	client *http.Client
}

var wechatIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var wechatTradeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func NewWechat(c WechatConfig) (Provider, error) {
	if !wechatIdentifier.MatchString(c.AppID) || !wechatIdentifier.MatchString(c.MchID) || !wechatIdentifier.MatchString(c.Serial) || !wechatIdentifier.MatchString(c.PlatformSerial) || len(c.APIv3Key) != 32 || c.PrivateKey == nil || c.PlatformKey == nil || c.PrivateKey.N == nil || c.PlatformKey.N == nil || c.PrivateKey.N.BitLen() < 2048 || c.PlatformKey.N.BitLen() < 2048 || c.PrivateKey.Validate() != nil {
		return nil, errors.New("invalid WeChat merchant or key configuration")
	}
	if c.Gateway == "" {
		c.Gateway = "https://api.mch.weixin.qq.com"
	}
	u, err := url.Parse(c.Gateway)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.Gateway, "#") {
		return nil, errors.New("invalid WeChat HTTPS gateway origin")
	}
	c.Gateway = strings.TrimRight(c.Gateway, "/")
	if c.Now == nil {
		c.Now = time.Now
	}
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	if client.Timeout <= 0 || client.Timeout > 8*time.Second {
		client.Timeout = 8 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &wechatProvider{config: c, client: &client}, nil
}
func (*wechatProvider) ID() string   { return "wechat" }
func (*wechatProvider) Name() string { return "微信支付" }

func wechatToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, c := range value {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (w *wechatProvider) verifyHeaders(header http.Header, body []byte) error {
	values := make([]string, 4)
	for i, key := range []string{"Wechatpay-Timestamp", "Wechatpay-Nonce", "Wechatpay-Serial", "Wechatpay-Signature"} {
		all := header.Values(key)
		if len(all) != 1 {
			return ErrVerification
		}
		values[i] = all[0]
	}
	timestamp, nonce, serial, signature := values[0], values[1], values[2], values[3]
	if len(timestamp) > 12 || !wechatToken(nonce, 128) || serial != w.config.PlatformSerial {
		return ErrVerification
	}
	for _, c := range timestamp {
		if c < '0' || c > '9' {
			return ErrVerification
		}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || seconds <= 0 {
		return ErrVerification
	}
	delta := w.config.Now().Unix() - seconds
	if delta < -300 || delta > 300 {
		return ErrVerification
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(signature)
	if err != nil {
		return ErrVerification
	}
	message := append([]byte(timestamp+"\n"+nonce+"\n"), body...)
	message = append(message, '\n')
	digest := sha256.Sum256(message)
	if rsa.VerifyPKCS1v15(w.config.PlatformKey, crypto.SHA256, digest[:], decoded) != nil {
		return ErrVerification
	}
	return nil
}

func (w *wechatProvider) call(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return w.callStatus(ctx, method, path, body, http.StatusOK)
}
func (w *wechatProvider) signedRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, ErrGateway
	}
	nonce := hex.EncodeToString(nonceBytes)
	timestamp := strconv.FormatInt(w.config.Now().Unix(), 10)
	message := method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, w.config.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return nil, ErrGateway
	}
	r, err := http.NewRequestWithContext(ctx, method, w.config.Gateway+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrGateway
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Wechatpay-Serial", w.config.PlatformSerial)
	r.Header.Set("Authorization", fmt.Sprintf(`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",timestamp="%s",serial_no="%s",signature="%s"`, w.config.MchID, nonce, timestamp, w.config.Serial, base64.StdEncoding.EncodeToString(signature)))
	return r, nil
}

func (w *wechatProvider) callStatus(ctx context.Context, method, path string, body []byte, successStatus int) ([]byte, error) {
	r, err := w.signedRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	response, err := w.client.Do(r)
	if err != nil {
		return nil, ErrGateway
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, ErrGateway
	}
	if err = w.verifyHeaders(response.Header, data); err != nil {
		return nil, ErrGateway
	}
	if response.StatusCode == http.StatusNotFound {
		var failure struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(data, &failure) == nil && ((!strings.HasPrefix(path, "/v3/refund/domestic/refunds/") && failure.Code == "ORDER_NOT_EXIST") || (method == http.MethodGet && strings.HasPrefix(path, "/v3/refund/domestic/refunds/") && failure.Code == "RESOURCE_NOT_EXISTS")) {
			return nil, ErrNotPaid
		}
	}
	if response.StatusCode != successStatus {
		return nil, ErrGateway
	}
	return data, nil
}

func wechatValidRequest(r Request) bool {
	return wechatTradeID.MatchString(r.PaymentID) && r.Amount > 0 && r.Amount <= math.MaxInt32 && r.Currency == "CNY"
}
func (w *wechatProvider) Create(ctx context.Context, r Request) (Checkout, error) {
	if !wechatValidRequest(r) || !utf8.ValidString(r.Name) || r.Name == "" {
		return Checkout{}, ErrGateway
	}
	notify, err := url.Parse(r.NotifyURL)
	if err != nil || notify.Scheme != "https" || notify.Hostname() == "" || notify.User != nil || strings.Contains(r.NotifyURL, "#") {
		return Checkout{}, ErrGateway
	}
	name := r.Name
	for len(name) > 127 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	body, err := json.Marshal(map[string]any{"appid": w.config.AppID, "mchid": w.config.MchID, "description": name, "out_trade_no": r.PaymentID, "notify_url": r.NotifyURL, "amount": map[string]any{"total": r.Amount, "currency": r.Currency}})
	if err != nil {
		return Checkout{}, ErrGateway
	}
	data, err := w.call(ctx, http.MethodPost, "/v3/pay/transactions/native", body)
	if err != nil {
		return Checkout{}, err
	}
	var result struct {
		URL string `json:"code_url"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.URL) > 4096 {
		return Checkout{}, ErrGateway
	}
	code, err := url.Parse(result.URL)
	if err != nil || code.Scheme != "weixin" || code.Host != "wxpay" || code.Path != "/bizpayurl" || code.User != nil || code.RawQuery == "" || code.Fragment != "" {
		return Checkout{}, ErrGateway
	}
	return Checkout{Kind: "qr", URL: result.URL}, nil
}

type wechatTransaction struct {
	AppID         string `json:"appid"`
	MchID         string `json:"mchid"`
	PaymentID     string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	State         string `json:"trade_state"`
	Amount        struct {
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

func (w *wechatProvider) transaction(data []byte) (Notification, error) {
	var t wechatTransaction
	if json.Unmarshal(data, &t) != nil || t.AppID != w.config.AppID || t.MchID != w.config.MchID || !wechatTradeID.MatchString(t.PaymentID) || t.Amount.Total <= 0 || t.Amount.Total > math.MaxInt32 || t.Amount.Currency != "CNY" {
		return Notification{}, ErrVerification
	}
	paid := t.State == "SUCCESS"
	if paid && !wechatToken(t.TransactionID, 128) {
		return Notification{}, ErrVerification
	}
	switch t.State {
	case "SUCCESS", "REFUND", "NOTPAY", "CLOSED", "REVOKED", "USERPAYING", "PAYERROR":
	default:
		return Notification{}, ErrVerification
	}
	return Notification{PaymentID: t.PaymentID, TransactionID: t.TransactionID, Amount: t.Amount.Total, Currency: t.Amount.Currency, Paid: paid, Closed: t.State == "CLOSED"}, nil
}
func (w *wechatProvider) Verify(r *http.Request) (Notification, error) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return Notification{}, ErrVerification
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(data) > 65536 {
		return Notification{}, ErrVerification
	}
	if err = w.verifyHeaders(r.Header, data); err != nil {
		return Notification{}, err
	}
	var event struct {
		ID           string `json:"id"`
		Type         string `json:"event_type"`
		ResourceType string `json:"resource_type"`
		Resource     struct {
			OriginalType   string `json:"original_type"`
			Algorithm      string `json:"algorithm"`
			Ciphertext     string `json:"ciphertext"`
			Nonce          string `json:"nonce"`
			AssociatedData string `json:"associated_data"`
		} `json:"resource"`
	}
	if json.Unmarshal(data, &event) != nil || event.ID == "" || event.Type != "TRANSACTION.SUCCESS" || event.ResourceType != "encrypt-resource" || event.Resource.OriginalType != "transaction" || event.Resource.Algorithm != "AEAD_AES_256_GCM" {
		return Notification{}, ErrVerification
	}
	block, err := aes.NewCipher([]byte(w.config.APIv3Key))
	if err != nil {
		return Notification{}, ErrVerification
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Notification{}, ErrVerification
	}
	if len(event.Resource.Nonce) != gcm.NonceSize() {
		return Notification{}, ErrVerification
	}
	ciphertext, err := base64.StdEncoding.Strict().DecodeString(event.Resource.Ciphertext)
	if err != nil {
		return Notification{}, ErrVerification
	}
	plaintext, err := gcm.Open(nil, []byte(event.Resource.Nonce), ciphertext, []byte(event.Resource.AssociatedData))
	if err != nil {
		return Notification{}, ErrVerification
	}
	n, err := w.transaction(plaintext)
	if err != nil || !n.Paid {
		return Notification{}, ErrVerification
	}
	return n, nil
}
func (w *wechatProvider) Query(ctx context.Context, r Request) (Notification, error) {
	if !wechatValidRequest(r) {
		return Notification{}, ErrGateway
	}
	path := "/v3/pay/transactions/out-trade-no/" + url.PathEscape(r.PaymentID) + "?mchid=" + url.QueryEscape(w.config.MchID)
	data, err := w.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Notification{}, err
	}
	n, err := w.transaction(data)
	if err != nil || n.PaymentID != r.PaymentID || n.Amount != r.Amount || n.Currency != r.Currency {
		return Notification{}, ErrVerification
	}
	return n, nil
}
