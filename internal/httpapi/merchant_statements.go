package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
)

const statementImportBodyLimit = 2 << 20

func statementValidationMessage(err error) string {
	fallback := "账单格式不正确，请按标准 CSV 模板核对日期、标识、金额和行数"
	var validation *statements.ValidationError
	if !errors.As(err, &validation) || validation.Line < 0 || validation.Line > statements.MaxNativeExpandedBytes+1 {
		return fallback
	}
	fields := map[string]string{
		"provider": "支付渠道", "kind": "账单类型", "date": "账单日期", "coverage": "账单范围",
		"csv": "CSV 文件", "header": "账单表头", "payment_id": "商户支付单号",
		"transaction_id": "渠道交易号", "refund_id": "商户退款单号", "provider_refund_id": "渠道退款号",
		"amount_minor": "金额（分）", "currency": "币种", "occurred_at": "发生时间",
		"format": "原始账单版本", "content_base64": "原始账单文件",
		"fee_yuan": "手续费（元）", "channel_amount_yuan": "渠道金额（元）",
	}
	field, known := fields[validation.Field]
	if !known {
		return fallback
	}
	message := ""
	switch validation.Code {
	case "invalid_field":
		message = field + "格式不正确"
	case "too_large":
		message = "CSV 文件不能超过 1 MiB"
	case "too_many_rows":
		message = "账单不能超过 2000 条记录"
	case "invalid_utf8":
		message = "CSV 文件必须使用有效的 UTF-8 编码"
	case "invalid_control":
		message = "CSV 文件不能包含控制字符或单独的回车符"
	case "blank_record":
		message = "账单不能包含空白记录"
	case "invalid_header":
		message = "账单表头必须与标准模板完全一致"
	case "invalid_record":
		message = "账单记录必须包含格式正确的 7 列"
	case "unsupported_refund_provider":
		message = "退款账单暂仅支持支付宝和微信支付"
	case "unexpected_refund_reference":
		message = "收款账单的退款编号字段必须为空"
	case "duplicate_identity":
		message = field + "出现重复标识"
	case "amount_overflow":
		message = "账单金额或金额合计超出支持范围"
	case "outside_date":
		message = "发生时间不在所选北京时间日期内"
	case "unsupported_native_format":
		message = "不支持此原始账单版本或渠道类型，请使用支持的版本或标准 CSV"
	case "invalid_native_record":
		message = "原始账单记录格式不正确，请核对文件编码、字段和金额"
	case "invalid_native_summary":
		message = "原始账单汇总缺失或与明细不一致，不能确认导入范围"
	case "native_merchant_mismatch":
		message = "原始账单商户信息不一致或包含暂不支持的子商户范围"
	case "native_archive_invalid":
		message = "原始账单压缩文件不完整、结构不支持或校验未通过"
	case "native_too_large":
		message = "原始账单文件或解压内容不能超过 8 MiB"
	case "native_empty":
		message = "无法确认空账单，请核对原始文件和支持的账单版本"
	case "native_partial_only":
		message = "此支付宝原始账单版本仅支持按部分账单核对"
	case "financial_amount_invalid":
		message = field + "必须为明确填写的十进制金额，最多 5 位小数，不使用科学计数法"
	case "financial_amount_overflow":
		message = "手续费、渠道金额或金额合计超出支持范围"
	case "financial_document_invalid":
		message = "来源金额观察未通过格式或归属校验"
	case "financial_reference_header":
		message = "预期金额 CSV 表头必须包含 payment_id、refund_id、expected_fee_yuan、expected_channel_amount_yuan 四列"
	case "financial_reference_record":
		message = "预期金额记录必须包含正确的支付/退款标识及明确填写的手续费和渠道金额"
	default:
		return fallback
	}
	if validation.Line > 0 {
		return fmt.Sprintf("账单第 %d 行：%s", validation.Line, message)
	}
	return message
}

func (s *Server) statementError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, statements.ErrInvalid):
		// Parser errors may identify a finite field or row, but untrusted file
		// contents must never become an error or log payload.
		fail(w, http.StatusBadRequest, statementValidationMessage(err))
	case errors.Is(err, store.ErrRoleForbidden):
		fail(w, http.StatusForbidden, "后台权限已变化，请重新登录")
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "账单、对账版本或差异记录不存在")
	case errors.Is(err, store.ErrStatementConflict):
		fail(w, http.StatusConflict, "对账数据超出本次处理范围或正在变化，请缩小范围或刷新后重试")
	default:
		fail(w, http.StatusServiceUnavailable, "账单对账服务暂时不可用，请稍后重试")
	}
}

func statementNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "此账单接口不支持筛选参数")
		return false
	}
	return true
}

func statementPathIDs(w http.ResponseWriter, r *http.Request, names ...string) bool {
	for _, name := range names {
		if !paymentOperationID.MatchString(r.PathValue(name)) {
			fail(w, http.StatusNotFound, "账单、对账版本或差异记录不存在")
			return false
		}
	}
	return true
}

func (s *Server) statementWriteUser(w http.ResponseWriter, r *http.Request, purpose string, limit int) (store.User, bool) {
	u, ok := s.current(w, r, true)
	if !ok {
		return u, false
	}
	ip := clientAddress(r)
	// Keep per-peer and per-user limits independent. In particular a limiter
	// storage error is unavailable, rather than a misleading rate-limit result.
	for _, key := range []string{"statement-peer:" + purpose + ":" + ip, "statement-user:" + purpose + ":" + u.ID} {
		allowed, err := s.Store.AllowRate(r.Context(), key, limit, time.Minute)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "账单对账服务暂时不可用，请稍后重试")
			return u, false
		}
		if !allowed {
			fail(w, http.StatusTooManyRequests, "账单操作过于频繁，请稍后重试")
			return u, false
		}
	}
	return u, true
}

func decodeStatementImport(w http.ResponseWriter, r *http.Request, input *statements.Import) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, statementImportBodyLimit))
	d.DisallowUnknownFields()
	if err := d.Decode(input); err != nil {
		fail(w, http.StatusBadRequest, "请提交不超过 2 MiB 的单个标准账单 JSON 对象")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) adminStatements(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	if !statementNoQuery(w, r) {
		return
	}
	items, err := s.Store.ListStatements(r.Context())
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) importStatement(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "import", 5)
	if !ok || !statementNoQuery(w, r) {
		return
	}
	var input statements.Import
	if !decodeStatementImport(w, r, &input) {
		return
	}
	detail, err := s.Store.ImportStatement(r.Context(), u.ID, input)
	if err != nil {
		s.statementError(w, err)
		return
	}
	// A user-uploaded statement is evidence for comparison only. This handler
	// deliberately has no merchant adapter or payment fulfillment operation.
	reply(w, http.StatusOK, detail)
}

func (s *Server) adminStatement(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	if !statementNoQuery(w, r) || !statementPathIDs(w, r, "id") {
		return
	}
	detail, err := s.Store.Statement(r.Context(), r.PathValue("id"))
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, detail)
}

func (s *Server) reconcileStatement(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "reconcile", 5)
	if !ok || !statementNoQuery(w, r) || !statementPathIDs(w, r, "id") {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	run, err := s.Store.ReconcileStatement(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, run)
}

func statementFindingQuery(raw string) (review string, offset, limit int, valid bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return "", 0, 0, false
	}
	review, limit = "all", 100
	for key, values := range query {
		if len(values) != 1 || values[0] == "" {
			return "", 0, 0, false
		}
		value := values[0]
		switch key {
		case "review":
			if value != "all" && value != "unreviewed" && value != "reviewed" {
				return "", 0, 0, false
			}
			review = value
		case "offset", "limit":
			if strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
				return "", 0, 0, false
			}
			number, err := strconv.Atoi(value)
			if err != nil {
				return "", 0, 0, false
			}
			if key == "offset" {
				if number > 4000 {
					return "", 0, 0, false
				}
				offset = number
			} else {
				if number < 1 || number > 100 {
					return "", 0, 0, false
				}
				limit = number
			}
		default:
			return "", 0, 0, false
		}
	}
	return review, offset, limit, true
}

func (s *Server) statementFindings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	if !statementPathIDs(w, r, "id", "runID") {
		return
	}
	review, offset, limit, valid := statementFindingQuery(r.URL.RawQuery)
	if !valid {
		fail(w, http.StatusBadRequest, "差异筛选条件不正确，请使用有效的复核状态和分页范围")
		return
	}
	page, err := s.Store.StatementFindings(r.Context(), r.PathValue("id"), r.PathValue("runID"), review, offset, limit)
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, page)
}

func (s *Server) statementFindingReviews(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	if !statementNoQuery(w, r) || !statementPathIDs(w, r, "id") {
		return
	}
	items, err := s.Store.StatementFindingReviews(r.Context(), r.PathValue("id"))
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) reviewStatementFinding(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "review", 10)
	if !ok || !statementNoQuery(w, r) || !statementPathIDs(w, r, "id") {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	note := strings.TrimSpace(body.Note)
	if note == "" || len(note) > 1000 || !utf8.ValidString(note) || strings.IndexFunc(note, unicode.IsControl) >= 0 {
		fail(w, http.StatusBadRequest, "请填写 1–1000 字节的复核备注，不能包含控制字符")
		return
	}
	review, err := s.Store.ReviewStatementFinding(r.Context(), u.ID, r.PathValue("id"), note)
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, review)
}
