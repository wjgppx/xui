package payments

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (a *alipayProvider) BillMerchantFingerprint() string {
	return billFingerprint(a.ID(), a.config.AppID, a.config.SellerID)
}

func (a *alipayProvider) billMetadata(ctx context.Context, request BillRequest) (string, error) {
	parameters, err := a.parameters("alipay.data.dataservice.bill.downloadurl.query", map[string]string{"bill_type": "trade", "bill_date": request.Date, "secure": "true"})
	if err != nil {
		return "", err
	}
	if err = a.sign(parameters); err != nil {
		return "", err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.Gateway, strings.NewReader(parameters.Encode()))
	if err != nil {
		return "", ErrGateway
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(r)
	if err != nil {
		return "", ErrGateway
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || response.StatusCode != http.StatusOK {
		return "", ErrGateway
	}
	payload, signature, err := alipayNamedEnvelope(data, "alipay_data_dataservice_bill_downloadurl_query_response")
	if err != nil {
		return "", err
	}
	if !a.verify(payload, signature) {
		return "", ErrVerification
	}
	var metadata struct {
		Code         string `json:"code"`
		AppID        string `json:"app_id"`
		SellerID     string `json:"seller_id"`
		DownloadURL  string `json:"bill_download_url"`
		BillFileCode string `json:"bill_file_code"`
	}
	if json.Unmarshal(payload, &metadata) != nil {
		return "", ErrGateway
	}
	if (metadata.AppID != "" && metadata.AppID != a.config.AppID) || (metadata.SellerID != "" && metadata.SellerID != a.config.SellerID) {
		return "", ErrVerification
	}
	// No undocumented code or empty-data marker is converted into an empty
	// imported bill. Generic bounded retry is safer than inventing readiness.
	if metadata.Code != "10000" || metadata.DownloadURL == "" {
		return "", ErrGateway
	}
	return metadata.DownloadURL, nil
}

func (a *alipayProvider) DownloadBill(ctx context.Context, request BillRequest) (Bill, error) {
	if !validBillKind(request.Kind) {
		return Bill{}, ErrBillUnsupported
	}
	if !ValidBillDate(request.Date, a.config.Now()) {
		return Bill{}, ErrGateway
	}
	rawURL, err := a.billMetadata(ctx, request)
	if err != nil {
		return Bill{}, err
	}
	u, err := parseBillURL(rawURL, "/downloadBillFile.resource")
	if err != nil {
		return Bill{}, err
	}
	allowed := strings.EqualFold(u.Host, "dwbillcenter.alipay.com") && u.Port() == ""
	// An explicit fixture client can reach the same local TLS origin as its
	// configured API gateway. Environment-loaded providers have no such client.
	if !allowed && a.config.HTTPClient != nil {
		gateway, parseErr := url.Parse(a.config.Gateway)
		allowed = parseErr == nil && sameBillOrigin(u, gateway)
	}
	if !allowed {
		return Bill{}, ErrVerification
	}
	if u.RawQuery == "" {
		return Bill{}, ErrVerification
	}
	if _, err = url.ParseQuery(u.RawQuery); err != nil {
		return Bill{}, ErrVerification
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Bill{}, ErrGateway
	}
	r.Header.Set("Accept", "application/zip")
	r.Header.Set("Accept-Encoding", "identity")
	client := *a.client
	client.Jar = nil // A CDN download must not carry API cookies or credentials.
	response, err := client.Do(r)
	if err != nil {
		return Bill{}, ErrGateway
	}
	data, err := readBillBody(response)
	if err != nil {
		return Bill{}, err
	}
	if len(data) < 4 || !bytes.Equal(data[:4], []byte{'P', 'K', 3, 4}) {
		return Bill{}, ErrGateway
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) == 0 || len(archive.File) > 8 {
		return Bill{}, ErrGateway
	}
	var expanded uint64
	for _, file := range archive.File {
		if file.UncompressedSize64 > MaxBillBytes || expanded > MaxBillBytes-file.UncompressedSize64 {
			return Bill{}, ErrGateway
		}
		expanded += file.UncompressedSize64
	}
	return Bill{Provider: a.ID(), Date: request.Date, Kind: request.Kind, Format: "alipay_trade_v1", MerchantID: a.config.SellerID, Data: data}, nil
}

var _ BillProvider = (*alipayProvider)(nil)
