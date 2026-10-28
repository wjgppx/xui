package payments

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func (w *wechatProvider) BillMerchantFingerprint() string {
	return billFingerprint(w.ID(), w.config.AppID, w.config.MchID)
}

func (w *wechatProvider) DownloadBill(ctx context.Context, request BillRequest) (Bill, error) {
	if !validBillKind(request.Kind) {
		return Bill{}, ErrBillUnsupported
	}
	if !ValidBillDate(request.Date, w.config.Now()) {
		return Bill{}, ErrGateway
	}
	typeName, format := "SUCCESS", "wechat_success_v1"
	if request.Kind == "refund" {
		typeName, format = "REFUND", "wechat_refund_v1"
	}
	query := url.Values{"bill_date": {request.Date}, "bill_type": {typeName}, "tar_type": {"GZIP"}}
	data, err := w.call(ctx, http.MethodGet, "/v3/bill/tradebill?"+query.Encode(), nil)
	if err != nil {
		return Bill{}, err
	}
	var metadata struct {
		URL      string `json:"download_url"`
		HashType string `json:"hash_type"`
		Hash     string `json:"hash_value"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return Bill{}, ErrGateway
	}
	if metadata.HashType != "SHA1" || len(metadata.Hash) != sha1.Size*2 {
		return Bill{}, ErrVerification
	}
	expected, err := hex.DecodeString(metadata.Hash)
	if err != nil {
		return Bill{}, ErrVerification
	}
	u, err := parseBillURL(metadata.URL, "/v3/billdownload/file")
	if err != nil {
		return Bill{}, err
	}
	gateway, err := url.Parse(w.config.Gateway)
	if err != nil || !sameBillOrigin(u, gateway) {
		return Bill{}, ErrVerification
	}
	parameters, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(parameters) != 1 || len(parameters["token"]) != 1 || !wechatToken(parameters.Get("token"), 4096) {
		return Bill{}, ErrVerification
	}
	r, err := w.signedRequest(ctx, http.MethodGet, u.RequestURI(), nil)
	if err != nil {
		return Bill{}, err
	}
	r.Header.Set("Accept", "*/*")
	r.Header.Set("Accept-Encoding", "identity")
	response, err := w.client.Do(r)
	if err != nil {
		return Bill{}, ErrGateway
	}
	raw, err := readBillBody(response)
	if err != nil {
		return Bill{}, err
	}
	plain, err := expandWechatBill(raw)
	if err != nil {
		return Bill{}, err
	}
	digest := sha1.Sum(plain) // Protocol checksum of uncompressed bytes, not trust discovery.
	if !strings.EqualFold(hex.EncodeToString(digest[:]), hex.EncodeToString(expected)) {
		return Bill{}, ErrVerification
	}
	return Bill{Provider: w.ID(), Date: request.Date, Kind: request.Kind, Format: format, MerchantID: w.config.MchID, Data: plain}, nil
}

var _ BillProvider = (*wechatProvider)(nil)
