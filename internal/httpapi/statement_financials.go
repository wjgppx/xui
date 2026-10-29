package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
)

// Financial observations and reference comparisons have the same authority as
// the underlying imported source: they never confirm a money movement.
func (s *Server) sourceFinancials(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !statementNoQuery(w, r) || !statementPathIDs(w, r, "id", "sourceID") {
		return
	}
	snapshot, err := s.Store.SourceFinancials(r.Context(), r.PathValue("id"), r.PathValue("sourceID"))
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, snapshot)
}

func decodeFinancialReference(w http.ResponseWriter, r *http.Request, csv *string) bool {
	var input struct {
		CSV *string `json:"csv"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, statementImportBodyLimit))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil || input.CSV == nil {
		fail(w, http.StatusBadRequest, "请提交不超过 2 MiB 的单个预期金额 JSON 对象，并明确填写 CSV 内容")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	*csv = *input.CSV
	return true
}

func (s *Server) reconcileSourceFinancials(w http.ResponseWriter, r *http.Request) {
	u, ok := s.statementWriteUser(w, r, "financial-reconcile", 5)
	if !ok || !statementNoQuery(w, r) || !statementPathIDs(w, r, "id", "sourceID") {
		return
	}
	var csv string
	if !decodeFinancialReference(w, r, &csv) {
		return
	}
	report, err := s.Store.ReconcileSourceFinancials(r.Context(), u.ID, r.PathValue("id"), r.PathValue("sourceID"), csv, u.SecurityVersion)
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, report)
}

func (s *Server) sourceFinancialReport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok || !statementNoQuery(w, r) || !statementPathIDs(w, r, "id", "sourceID", "runID") {
		return
	}
	report, err := s.Store.SourceFinancialReport(r.Context(), r.PathValue("id"), r.PathValue("sourceID"), r.PathValue("runID"))
	if err != nil {
		s.statementError(w, err)
		return
	}
	reply(w, http.StatusOK, report)
}
