package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/wjgppx/xui/internal/fundledger"
	"github.com/wjgppx/xui/internal/store"
)

const fundLedgerBodyLimit = 2 << 20

func decodeFundLedgerRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, fundLedgerBodyLimit))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		fail(w, http.StatusBadRequest, "请提交不超过 2 MiB 的单个资金流水 JSON 对象")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func fundLedgerNoQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, http.StatusBadRequest, "资金流水接口不支持查询参数")
		return false
	}
	return true
}

func fundLedgerID(w http.ResponseWriter, id string) bool {
	if !paymentOperationID.MatchString(id) {
		fail(w, http.StatusBadRequest, "资金流水或核对报告标识格式不正确")
		return false
	}
	return true
}

func fundLedgerValidationMessage(err error) string {
	fallback := "资金流水格式不正确，请核对账户、日期、金额、顺序和 CSV 模板"
	var validation *fundledger.ValidationError
	if !errors.As(err, &validation) || validation.Line < 0 || validation.Line > (1<<20)+1 {
		return fallback
	}
	fields := map[string]string{
		"provider": "账户来源", "account_key": "账户别名", "date": "流水日期", "coverage": "流水范围",
		"opening_balance_yuan": "期初余额", "closing_balance_yuan": "期末余额", "csv": "CSV 文件",
		"sequence": "流水顺序", "entry_id": "流水编号", "occurred_at": "发生时间", "kind": "流水类型",
		"direction": "收支方向", "amount_yuan": "流水金额", "balance_after_yuan": "流水后余额",
		"transfer_id": "转账引用", "counterparty_account_key": "对方账户别名",
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
		message = "资金流水 CSV 不能超过 1 MiB"
	case "too_many_rows":
		message = "资金流水不能超过 2000 条记录"
	case "invalid_utf8":
		message = "资金流水 CSV 必须使用有效 UTF-8 编码"
	case "invalid_control", "blank_record":
		message = "资金流水 CSV 不能包含控制字符、空白记录或单独回车"
	case "invalid_header", "invalid_record":
		message = "资金流水 CSV 必须使用规定的九列表头和记录"
	case "invalid_sequence":
		message = "流水顺序必须从 1 开始连续递增，不要重新排列原始余额顺序"
	case "duplicate_entry_id":
		message = "资金流水编号重复"
	case "invalid_order":
		message = "流水发生时间必须按顺序排列，同一时间可保留原始顺序"
	case "outside_date":
		message = "流水发生时间不在所选北京时间日期内"
	case "invalid_amount":
		message = field + "必须为精确元金额，最多五位小数，流水金额须大于零"
	case "amount_overflow":
		message = "资金流水金额或合计超出支持范围"
	case "invalid_pair":
		message = "请选择不同账户的商户流水和银行流水"
	case "invalid_document":
		return fallback
	default:
		return fallback
	}
	if validation.Line > 0 {
		return fmt.Sprintf("资金流水第 %d 行：%s", validation.Line, message)
	}
	return message
}

func (s *Server) fundLedgerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fundledger.ErrInvalid):
		fail(w, http.StatusBadRequest, fundLedgerValidationMessage(err))
	case errors.Is(err, store.ErrRoleForbidden):
		fail(w, http.StatusForbidden, "财务权限或会话已变化，请重新登录")
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "资金流水批次或核对报告不存在")
	case errors.Is(err, store.ErrFundAccountConflict):
		fail(w, http.StatusConflict, "账户别名已绑定其他来源，请核对后使用不同别名")
	case errors.Is(err, store.ErrFundLedgerConflict):
		fail(w, http.StatusConflict, "资金流水范围或核对对象不兼容，或数据正在变化，请核对后重试")
	default:
		fail(w, http.StatusServiceUnavailable, "资金流水服务暂时不可用，请稍后重试")
	}
}

func (s *Server) fundLedgerWriteUser(w http.ResponseWriter, r *http.Request, purpose string) (store.User, bool) {
	u, ok := s.current(w, r, true)
	if !ok {
		return u, false
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	for _, key := range []string{"fund-peer:" + purpose + ":" + ip, "fund-user:" + purpose + ":" + u.ID} {
		allowed, err := s.Store.AllowRate(r.Context(), key, 5, time.Minute)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "资金流水服务暂时不可用，请稍后重试")
			return u, false
		}
		if !allowed {
			fail(w, http.StatusTooManyRequests, "资金流水操作过于频繁，请稍后重试")
			return u, false
		}
	}
	return u, true
}

func (s *Server) adminFundLedgers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !fundLedgerNoQuery(w, r) {
		return
	}
	items, err := s.Store.ListFundLedgers(r.Context())
	if err != nil {
		s.fundLedgerError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) importFundLedger(w http.ResponseWriter, r *http.Request) {
	u, ok := s.fundLedgerWriteUser(w, r, "fund-ledger-import")
	if !ok || !fundLedgerNoQuery(w, r) {
		return
	}
	var input fundledger.Import
	if !decodeFundLedgerRequest(w, r, &input) {
		return
	}
	ledger, err := s.Store.ImportFundLedger(r.Context(), u.ID, input, u.SecurityVersion)
	if err != nil {
		s.fundLedgerError(w, err)
		return
	}
	// Manual bank and merchant files are observations. Arithmetic consistency
	// and transfer matches cannot settle a payment or prove bank arrival.
	reply(w, http.StatusOK, ledger)
}

func (s *Server) adminFundLedger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !fundLedgerNoQuery(w, r) || !fundLedgerID(w, r.PathValue("id")) {
		return
	}
	ledger, err := s.Store.FundLedger(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fundLedgerError(w, err)
		return
	}
	reply(w, http.StatusOK, ledger)
}

func (s *Server) adminFundReconciliations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !fundLedgerNoQuery(w, r) {
		return
	}
	items, err := s.Store.ListFundReconciliations(r.Context())
	if err != nil {
		s.fundLedgerError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) reconcileFundLedgers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.fundLedgerWriteUser(w, r, "fund-ledger-reconcile")
	if !ok || !fundLedgerNoQuery(w, r) {
		return
	}
	var input struct {
		MerchantBatchID string `json:"merchant_batch_id"`
		BankBatchID     string `json:"bank_batch_id"`
	}
	if !decodeFundLedgerRequest(w, r, &input) || !fundLedgerID(w, input.MerchantBatchID) || !fundLedgerID(w, input.BankBatchID) {
		return
	}
	report, err := s.Store.ReconcileFundLedgers(r.Context(), u.ID, input.MerchantBatchID, input.BankBatchID, u.SecurityVersion)
	if err != nil {
		s.fundLedgerError(w, err)
		return
	}
	reply(w, http.StatusOK, report)
}

func (s *Server) adminFundReconciliation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !fundLedgerNoQuery(w, r) || !fundLedgerID(w, r.PathValue("id")) {
		return
	}
	report, err := s.Store.FundReconciliation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fundLedgerError(w, err)
		return
	}
	reply(w, http.StatusOK, report)
}
