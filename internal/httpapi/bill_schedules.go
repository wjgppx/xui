package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

func (s *Server) adminBillSchedules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !statementNoQuery(w, r) {
		return
	}
	items, err := s.Store.ListBillSchedules(r.Context())
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) updateBillSchedule(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "bill-schedule", 5)
	if !ok || !statementNoQuery(w, r) {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
		Enabled  *bool  `json:"enabled"`
		RunAt    string `json:"run_at"`
		Revision int64  `json:"revision"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Every update names the saved revision and explicitly sets enabled. Missing
	// fields must not turn a malformed request into an accidental pause.
	_, timeErr := time.Parse("15:04", body.RunAt)
	if (body.Provider != "alipay" && body.Provider != "wechat") || (body.Kind != "payment" && body.Kind != "refund") || body.Enabled == nil || body.Revision < 1 || len(body.RunAt) != 5 || timeErr != nil {
		fail(w, http.StatusBadRequest, "请选择支付宝或微信收款/退款计划，填写北京时间 HH:MM，并提交当前计划版本和启停状态")
		return
	}
	fingerprint := ""
	if *body.Enabled {
		if !s.Config.BillDownloadsEnabled {
			fail(w, http.StatusServiceUnavailable, "官方账单下载尚未启用，仍可停用已有计划或手工导入账单")
			return
		}
		provider, configured := s.Config.Payments.Get(body.Provider)
		if !configured {
			fail(w, http.StatusServiceUnavailable, "该支付渠道尚未配置，暂时无法启用自动下载计划")
			return
		}
		billProvider, supported := provider.(payments.BillProvider)
		if !supported {
			fail(w, http.StatusConflict, "此渠道暂不支持官方账单下载")
			return
		}
		fingerprint = billProvider.BillMerchantFingerprint()
	}
	// Neither the browser nor this endpoint selects a merchant URL or calls the
	// gateway. The schedule retains only the server's current merchant identity.
	item, err := s.Store.UpdateBillSchedule(r.Context(), u.ID, store.BillScheduleInput{
		Provider: body.Provider, Kind: body.Kind, Enabled: *body.Enabled,
		RunAt: body.RunAt, Revision: body.Revision,
	}, fingerprint, u.SecurityVersion)
	if errors.Is(err, store.ErrStatementConflict) {
		fail(w, http.StatusConflict, "自动下载计划已发生变化，请刷新列表后重试")
		return
	}
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, item)
}
