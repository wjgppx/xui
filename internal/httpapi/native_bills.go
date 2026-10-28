package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/statements"
)

const nativeStatementBodyLimit = 12 << 20

func decodeNativeStatement(w http.ResponseWriter, r *http.Request, input *statements.NativeImport) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, nativeStatementBodyLimit))
	d.DisallowUnknownFields()
	if err := d.Decode(input); err != nil {
		fail(w, http.StatusBadRequest, "请提交不超过 12 MiB 的单个原始账单 JSON 对象")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) importNativeStatement(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "native-import", 5)
	if !ok || !statementNoQuery(w, r) {
		return
	}
	var input statements.NativeImport
	if !decodeNativeStatement(w, r, &input) {
		return
	}
	detail, err := s.Store.ImportNativeStatement(r.Context(), u.ID, input)
	if err != nil {
		s.statementError(w, err)
		return
	}
	// Native upload is an observation even when the file resembles an official
	// bill. Neither file contents nor source labels authenticate money movement.
	reply(w, http.StatusOK, detail)
}

func (s *Server) adminBillDownloads(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !statementNoQuery(w, r) {
		return
	}
	items, err := s.Store.ListBillDownloads(r.Context())
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) requestBillDownload(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "bill-download", 5)
	if !ok || !statementNoQuery(w, r) {
		return
	}
	if !s.Config.BillDownloadsEnabled {
		fail(w, http.StatusServiceUnavailable, "官方账单下载尚未启用，仍可手工导入账单")
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
		Date     string `json:"date"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Provider != "alipay" && body.Provider != "wechat" {
		fail(w, http.StatusConflict, "此渠道暂不支持官方账单下载")
		return
	}
	if (body.Kind != "payment" && body.Kind != "refund") || !payments.ValidBillDate(body.Date, time.Now()) {
		fail(w, http.StatusBadRequest, "请选择正确的账单类型，以及北京时间今天之前 90 天内的账单日期")
		return
	}
	provider, configured := s.Config.Payments.Get(body.Provider)
	if !configured {
		fail(w, http.StatusServiceUnavailable, "该支付渠道尚未配置，暂时无法下载官方账单")
		return
	}
	billProvider, supported := provider.(payments.BillProvider)
	if !supported {
		fail(w, http.StatusConflict, "此渠道暂不支持官方账单下载")
		return
	}
	// Only server-side merchant identity enters the job. URLs, tokens and
	// credentials are never accepted from this browser-facing endpoint, and
	// the HTTP request does not call the merchant or download any file.
	job, err := s.Store.RequestBillDownload(r.Context(), u.ID, body.Provider, body.Kind, body.Date, billProvider.BillMerchantFingerprint(), u.SecurityVersion)
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, job)
}
