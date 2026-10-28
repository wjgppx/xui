package payments

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AlipayConfig uses the application's signing key and Alipay's verification key.
// Certificate-mode serial numbers are not inferred from arbitrary PEM input.
type AlipayConfig struct {
	AppID, SellerID, Gateway string
	PrivateKey               *rsa.PrivateKey
	PublicKey                *rsa.PublicKey
	HTTPClient               *http.Client
	Now                      func() time.Time
}
type alipayProvider struct {
	config AlipayConfig
	client *http.Client
}

var alipayMerchantID = regexp.MustCompile(`^[0-9]{8,32}$`)
var alipayReference = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var alipayPaymentID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func NewAlipay(c AlipayConfig) (Provider, error) {
	if c.Gateway == "" {
		c.Gateway = "https://openapi.alipay.com/gateway.do"
	}
	u, err := url.Parse(c.Gateway)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.Gateway, "#") {
		return nil, errors.New("invalid Alipay gateway")
	}
	if c.HTTPClient == nil && c.Gateway != "https://openapi.alipay.com/gateway.do" && c.Gateway != "https://openapi-sandbox.dl.alipaydev.com/gateway.do" {
		return nil, errors.New("unsupported Alipay gateway")
	}
	if !alipayMerchantID.MatchString(c.AppID) || !alipayMerchantID.MatchString(c.SellerID) || c.PrivateKey == nil || c.PublicKey == nil || c.PrivateKey.N == nil || c.PublicKey.N == nil || c.PrivateKey.N.BitLen() < 2048 || c.PublicKey.N.BitLen() < 2048 || c.PublicKey.E < 3 {
		return nil, errors.New("invalid Alipay merchant or RSA keys")
	}
	if err = c.PrivateKey.Validate(); err != nil {
		return nil, errors.New("invalid Alipay signing key")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	client := http.Client{Timeout: 8 * time.Second}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
		if client.Timeout <= 0 || client.Timeout > 8*time.Second {
			client.Timeout = 8 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &alipayProvider{config: c, client: &client}, nil
}
func (a *alipayProvider) ID() string   { return "alipay" }
func (a *alipayProvider) Name() string { return "支付宝" }

// Notification signatures omit sign_type; signed API request parameters include
// it. Neither canonical representation URL-escapes values before hashing.
func alipayCanonical(values url.Values, notification bool) (string, error) {
	keys := make([]string, 0, len(values))
	for key, entries := range values {
		if len(entries) != 1 {
			return "", ErrVerification
		}
		if key != "sign" && (!notification || key != "sign_type") && entries[0] != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	return strings.Join(parts, "&"), nil
}
func (a *alipayProvider) sign(values url.Values) error {
	canonical, err := alipayCanonical(values, false)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(canonical))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.config.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return ErrGateway
	}
	values.Set("sign", base64.StdEncoding.EncodeToString(signature))
	return nil
}
func (a *alipayProvider) verify(content []byte, encoded string) bool {
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(content)
	return rsa.VerifyPKCS1v15(a.config.PublicKey, crypto.SHA256, digest[:], signature) == nil
}
func alipayMoney(amount int64) string {
	return strconv.FormatInt(amount/100, 10) + "." + strconv.FormatInt(amount%100/10, 10) + strconv.FormatInt(amount%10, 10)
}
func alipayMinor(value string) (int64, error) {
	// Alipay emits decimal yuan. Do not round floats, accept exponents, whitespace,
	// signs, extra precision, or a value that could overflow integer cents.
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 17 {
		return 0, ErrVerification
	}
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return 0, ErrVerification
		}
	}
	fraction := "00"
	if len(parts) == 2 {
		if len(parts[1]) < 1 || len(parts[1]) > 2 {
			return 0, ErrVerification
		}
		fraction = parts[1]
		for _, c := range fraction {
			if c < '0' || c > '9' {
				return 0, ErrVerification
			}
		}
		if len(fraction) == 1 {
			fraction += "0"
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ErrVerification
	}
	cents, _ := strconv.ParseInt(fraction, 10, 64)
	if whole > (int64(^uint64(0)>>1)-cents)/100 {
		return 0, ErrVerification
	}
	amount := whole*100 + cents
	if amount <= 0 {
		return 0, ErrVerification
	}
	return amount, nil
}
func alipayValidURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == ""
}
func (a *alipayProvider) parameters(method string, biz any) (url.Values, error) {
	data, err := json.Marshal(biz)
	if err != nil {
		return nil, ErrGateway
	}
	return url.Values{"app_id": {a.config.AppID}, "method": {method}, "format": {"JSON"}, "charset": {"utf-8"}, "sign_type": {"RSA2"}, "timestamp": {a.config.Now().In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02 15:04:05")}, "version": {"1.0"}, "biz_content": {string(data)}}, nil
}
func (a *alipayProvider) Create(ctx context.Context, r Request) (Checkout, error) {
	if ctx.Err() != nil {
		return Checkout{}, ctx.Err()
	}
	if !alipayPaymentID.MatchString(r.PaymentID) || r.Amount <= 0 || r.Currency != "CNY" || r.Name == "" || len(r.Name) > 256 || !alipayValidURL(r.NotifyURL) || !alipayValidURL(r.ReturnURL) {
		return Checkout{}, ErrGateway
	}
	values, err := a.parameters("alipay.trade.page.pay", map[string]string{"out_trade_no": r.PaymentID, "product_code": "FAST_INSTANT_TRADE_PAY", "total_amount": alipayMoney(r.Amount), "subject": r.Name, "seller_id": a.config.SellerID, "timeout_express": "30m"})
	if err != nil {
		return Checkout{}, err
	}
	values.Set("notify_url", r.NotifyURL)
	values.Set("return_url", r.ReturnURL)
	if err = a.sign(values); err != nil {
		return Checkout{}, err
	}
	return Checkout{Kind: "redirect", URL: a.config.Gateway + "?" + values.Encode()}, nil
}
func (a *alipayProvider) Verify(r *http.Request) (Notification, error) {
	if r.Method != "POST" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return Notification{}, ErrVerification
	}
	contentType := strings.Split(r.Header.Get("Content-Type"), ";")[0]
	if strings.TrimSpace(contentType) != "application/x-www-form-urlencoded" {
		return Notification{}, ErrVerification
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(data) > 65536 {
		return Notification{}, ErrVerification
	}
	values, err := url.ParseQuery(string(data))
	if err != nil {
		return Notification{}, ErrVerification
	}
	canonical, err := alipayCanonical(values, true)
	if err != nil || values.Get("sign_type") != "RSA2" || !a.verify([]byte(canonical), values.Get("sign")) || values.Get("app_id") != a.config.AppID || values.Get("seller_id") != a.config.SellerID {
		return Notification{}, ErrVerification
	}
	if currency := values.Get("trans_currency"); currency != "" && currency != "CNY" {
		return Notification{}, ErrVerification
	}
	amount, err := alipayMinor(values.Get("total_amount"))
	if err != nil {
		return Notification{}, ErrVerification
	}
	paymentID, transactionID := values.Get("out_trade_no"), values.Get("trade_no")
	if !alipayPaymentID.MatchString(paymentID) || !alipayReference.MatchString(transactionID) {
		return Notification{}, ErrVerification
	}
	status := values.Get("trade_status")
	if status != "TRADE_SUCCESS" && status != "TRADE_FINISHED" && status != "WAIT_BUYER_PAY" && status != "TRADE_CLOSED" {
		return Notification{}, ErrVerification
	}
	return Notification{PaymentID: paymentID, TransactionID: transactionID, Amount: amount, Currency: "CNY", Paid: status == "TRADE_SUCCESS" || status == "TRADE_FINISHED"}, nil
}

// Read the envelope without duplicate keys and preserve the exact signed JSON
// bytes; re-marshalling the response before verification would change its proof.
func alipayEnvelope(data []byte) (json.RawMessage, string, error) {
	return alipayNamedEnvelope(data, "alipay_trade_query_response")
}
func alipayNamedEnvelope(data []byte, responseName string) (json.RawMessage, string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, "", ErrGateway
	}
	seen := map[string]bool{}
	var payload json.RawMessage
	var signature string
	for decoder.More() {
		keyToken, e := decoder.Token()
		if e != nil {
			return nil, "", ErrGateway
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return nil, "", ErrGateway
		}
		seen[key] = true
		if key == responseName {
			if e = decoder.Decode(&payload); e != nil {
				return nil, "", ErrGateway
			}
		} else if key == "sign" {
			if e = decoder.Decode(&signature); e != nil {
				return nil, "", ErrGateway
			}
		} else {
			var discard json.RawMessage
			if e = decoder.Decode(&discard); e != nil {
				return nil, "", ErrGateway
			}
		}
	}
	if _, err = decoder.Token(); err != nil {
		return nil, "", ErrGateway
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || len(payload) == 0 || signature == "" {
		return nil, "", ErrGateway
	}
	return payload, signature, nil
}
func (a *alipayProvider) Query(ctx context.Context, r Request) (Notification, error) {
	if !alipayPaymentID.MatchString(r.PaymentID) || r.Amount <= 0 || r.Currency != "CNY" {
		return Notification{}, ErrGateway
	}
	values, err := a.parameters("alipay.trade.query", map[string]string{"out_trade_no": r.PaymentID})
	if err != nil {
		return Notification{}, err
	}
	if err = a.sign(values); err != nil {
		return Notification{}, err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", a.config.Gateway, strings.NewReader(values.Encode()))
	if err != nil {
		return Notification{}, ErrGateway
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(request)
	if err != nil {
		return Notification{}, ErrGateway
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 || response.StatusCode != http.StatusOK {
		return Notification{}, ErrGateway
	}
	payload, signature, err := alipayEnvelope(data)
	if err != nil {
		return Notification{}, err
	}
	if !a.verify(payload, signature) {
		return Notification{}, ErrVerification
	}
	var body struct {
		Code          string `json:"code"`
		SubCode       string `json:"sub_code"`
		OutTradeNo    string `json:"out_trade_no"`
		TradeNo       string `json:"trade_no"`
		TradeStatus   string `json:"trade_status"`
		TotalAmount   string `json:"total_amount"`
		TransCurrency string `json:"trans_currency"`
		SellerID      string `json:"seller_id"`
		AppID         string `json:"app_id"`
	}
	if err = json.Unmarshal(payload, &body); err != nil {
		return Notification{}, ErrGateway
	}
	if body.Code != "10000" {
		if body.SubCode == "ACQ.TRADE_NOT_EXIST" {
			return Notification{}, ErrNotPaid
		}
		return Notification{}, ErrGateway
	}
	if body.OutTradeNo != r.PaymentID || (body.AppID != "" && body.AppID != a.config.AppID) || (body.SellerID != "" && body.SellerID != a.config.SellerID) || (body.TransCurrency != "" && body.TransCurrency != "CNY") {
		return Notification{}, ErrVerification
	}
	amount, err := alipayMinor(body.TotalAmount)
	if err != nil || amount != r.Amount {
		return Notification{}, ErrVerification
	}
	if !alipayReference.MatchString(body.TradeNo) {
		return Notification{}, ErrVerification
	}
	if body.TradeStatus != "TRADE_SUCCESS" && body.TradeStatus != "TRADE_FINISHED" && body.TradeStatus != "WAIT_BUYER_PAY" && body.TradeStatus != "TRADE_CLOSED" {
		return Notification{}, ErrVerification
	}
	return Notification{PaymentID: r.PaymentID, TransactionID: body.TradeNo, Amount: amount, Currency: "CNY", Paid: body.TradeStatus == "TRADE_SUCCESS" || body.TradeStatus == "TRADE_FINISHED", Closed: body.TradeStatus == "TRADE_CLOSED"}, nil
}
