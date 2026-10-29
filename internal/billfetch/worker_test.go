package billfetch_test

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/billfetch"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type billProvider struct {
	fingerprint string
	download    func(payments.BillRequest) (payments.Bill, error)
}

func (*billProvider) ID() string   { return "wechat" }
func (*billProvider) Name() string { return "local verified bill fixture" }
func (*billProvider) Create(context.Context, payments.Request) (payments.Checkout, error) {
	return payments.Checkout{}, errors.New("unexpected payment create")
}
func (*billProvider) Verify(*http.Request) (payments.Notification, error) {
	return payments.Notification{}, payments.ErrVerification
}
func (*billProvider) Query(context.Context, payments.Request) (payments.Notification, error) {
	return payments.Notification{}, errors.New("unexpected payment query")
}
func (p *billProvider) BillMerchantFingerprint() string { return p.fingerprint }
func (p *billProvider) DownloadBill(_ context.Context, r payments.BillRequest) (payments.Bill, error) {
	return p.download(r)
}
func fixture(t *testing.T) (*store.Store, string, store.Payment, string) {
	t.Helper()
	ctx := context.Background()
	s := testutil.Database(t)
	actor := testutil.User(t, s)
	user := testutil.User(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	o, err := s.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePayment(ctx, user, o.ID, "wechat")
	if err != nil {
		t.Fatal(err)
	}
	date := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).AddDate(0, 0, -1).Format("2006-01-02")
	return s, actor, p, date
}
func nativeBill(t *testing.T, date, paymentID string) []byte {
	t.Helper()
	headers := []string{"交易时间", "公众账号ID", "商户号", "特约商户号", "设备号", "微信订单号", "商户订单号", "用户标识", "交易类型", "交易状态", "付款银行", "货币种类", "应结订单金额", "代金券金额", "商品名称", "商户数据包", "手续费", "费率", "订单金额", "费率备注"}
	data := []string{date + " 12:00:00", "wx-fixture", "1900000109", "0", "", "txn-" + paymentID, paymentID, "user", "NATIVE", "SUCCESS", "BANK", "CNY", "18.00", "1.00", "fixture", "", "0.00000", "0.60%", "19.00", ""}
	footer := []string{"总交易单数", "应结订单总金额", "退款总金额", "充值券退款总金额", "手续费总金额", "订单总金额", "申请退款总金额"}
	totals := []string{"1", "18.00", "0.00", "0.00", "0.00000", "19.00", "0.00"}
	for i := range data {
		data[i] = "`" + data[i]
	}
	for i := range totals {
		totals[i] = "`" + totals[i]
	}
	var out strings.Builder
	w := csv.NewWriter(&out)
	for _, record := range [][]string{headers, data, footer, totals} {
		if err := w.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	return []byte(out.String())
}
func registry(t *testing.T, p *billProvider) *payments.Registry {
	t.Helper()
	r, err := payments.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func process(t *testing.T, s *store.Store, r *payments.Registry) int {
	t.Helper()
	n, err := billfetch.Process(context.Background(), s, r, 1)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func queue(t *testing.T, s *store.Store, actor, date, fp string) store.BillDownloadJob {
	t.Helper()
	j, err := s.RequestBillDownload(context.Background(), actor, "wechat", "payment", date, fp)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestDownloadedBillImportsOnceWithoutSettlingItsObservedPayment(t *testing.T) {
	s, actor, p, date := fixture(t)
	fp := strings.Repeat("a", 64)
	job := queue(t, s, actor, date, fp)
	calls := 0
	r := registry(t, &billProvider{fingerprint: fp, download: func(req payments.BillRequest) (payments.Bill, error) {
		calls++
		if req.Date != date || req.Kind != "payment" {
			t.Error("job binding lost")
		}
		return payments.Bill{Provider: "wechat", Date: date, Kind: "payment", Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, date, p.ID)}, nil
	}})
	if process(t, s, r) != 1 || process(t, s, r) != 0 || calls != 1 {
		t.Fatal("download identity not stable")
	}
	jobs, err := s.ListBillDownloads(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Status != "succeeded" || jobs[0].StatementID == nil {
		t.Fatal(jobs, err)
	}
	detail, err := s.Statement(context.Background(), *jobs[0].StatementID)
	if err != nil || detail.Statement.Coverage != "full" || detail.LatestRun.DifferenceCount != 1 || len(detail.Sources) != 1 || detail.Sources[0].Integrity != "wechat_signed_sha1" || detail.Sources[0].Kind != "download" {
		t.Fatal("provenance or observation lost", detail, err)
	}
	financials, err := s.SourceFinancials(context.Background(), detail.Statement.ID, detail.Sources[0].ID)
	if err != nil || !financials.Available || financials.FeeTotalYuan == nil || *financials.FeeTotalYuan != "0.00000" || financials.ChannelAmountTotalYuan == nil || *financials.ChannelAmountTotalYuan != "18.00000" || len(financials.SummaryCodes) != 0 {
		t.Fatal("download did not atomically retain exact financial observations", financials, err)
	}
	var paymentStatus string
	var grants, events int
	err = s.DB.QueryRow(context.Background(), `SELECT p.status,(SELECT count(*) FROM entitlement_periods),(SELECT count(*) FROM payment_events) FROM payment_intents p WHERE id=$1`, p.ID).Scan(&paymentStatus, &grants, &events)
	if err != nil || paymentStatus != "pending" || grants != 0 || events != 0 {
		t.Fatal("bill became money authority", paymentStatus, grants, events, err)
	}
}

func TestBillMerchantAndActorChangesPreventAllExternalRequests(t *testing.T) {
	for _, reason := range []string{"merchant", "role", "security_version"} {
		t.Run(reason, func(t *testing.T) {
			s, actor, _, date := fixture(t)
			fp := strings.Repeat("a", 64)
			queue(t, s, actor, date, fp)
			current := fp
			if reason == "merchant" {
				current = strings.Repeat("b", 64)
			} else if reason == "role" {
				if _, err := s.DB.Exec(context.Background(), `UPDATE users SET role='user' WHERE id=$1`, actor); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.DB.Exec(context.Background(), `UPDATE users SET security_version=security_version+1 WHERE id=$1`, actor); err != nil {
					t.Fatal(err)
				}
			}
			r := registry(t, &billProvider{fingerprint: current, download: func(payments.BillRequest) (payments.Bill, error) {
				t.Error("obsolete authority reached merchant")
				return payments.Bill{}, payments.ErrGateway
			}})
			process(t, s, r)
			jobs, err := s.ListBillDownloads(context.Background())
			if err != nil || len(jobs) != 1 || jobs[0].Status != "manual_required" {
				t.Fatal(jobs, err)
			}
		})
	}
}

func TestBillFailuresAndWrongFileBindingNeverCreateAStatement(t *testing.T) {
	for _, reason := range []string{"gateway", "signature", "wrong_date", "wrong_kind", "wrong_provider", "wrong_merchant", "bad_csv"} {
		t.Run(reason, func(t *testing.T) {
			s, actor, p, date := fixture(t)
			fp := strings.Repeat("a", 64)
			queue(t, s, actor, date, fp)
			r := registry(t, &billProvider{fingerprint: fp, download: func(payments.BillRequest) (payments.Bill, error) {
				bill := payments.Bill{Provider: "wechat", Date: date, Kind: "payment", Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, date, p.ID)}
				switch reason {
				case "gateway":
					return payments.Bill{}, payments.ErrGateway
				case "signature":
					return payments.Bill{}, payments.ErrVerification
				case "wrong_date":
					bill.Date = "2000-01-01"
				case "wrong_kind":
					bill.Kind = "refund"
				case "wrong_provider":
					bill.Provider = "alipay"
				case "wrong_merchant":
					bill.MerchantID = "different"
				case "bad_csv":
					bill.Data = []byte("unrecognized file")
				}
				return bill, nil
			}})
			process(t, s, r)
			all, err := s.ListStatements(context.Background())
			if err != nil || len(all) != 0 {
				t.Fatal("untrusted result imported", all, err)
			}
		})
	}
}

func TestBillDownloadCrashBeforeCompletionRecoversWithNewClaim(t *testing.T) {
	s, actor, p, date := fixture(t)
	ctx := context.Background()
	fp := strings.Repeat("a", 64)
	job := queue(t, s, actor, date, fp)
	old, err := s.ClaimBillDownload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `UPDATE bill_download_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	r := registry(t, &billProvider{fingerprint: fp, download: func(payments.BillRequest) (payments.Bill, error) {
		return payments.Bill{Provider: "wechat", Date: date, Kind: "payment", Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, date, p.ID)}, nil
	}})
	process(t, s, r)
	if err = s.FinishBillDownload(ctx, job.ID, old.ClaimToken, "gateway_error"); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal("old worker completed recovered task", err)
	}
	all, err := s.ListStatements(ctx)
	if err != nil || len(all) != 1 {
		t.Fatal("recovery lost or duplicated statement", all, err)
	}
}

func TestBillCancelledContextDoesNotCallMerchant(t *testing.T) {
	s, actor, _, date := fixture(t)
	fp := strings.Repeat("a", 64)
	queue(t, s, actor, date, fp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := registry(t, &billProvider{fingerprint: fp, download: func(payments.BillRequest) (payments.Bill, error) {
		t.Error("cancelled worker called merchant")
		return payments.Bill{}, fmt.Errorf("unexpected")
	}})
	if _, err := billfetch.Process(ctx, s, r, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBillImportStorageFailureRollsBackAndRecoversByFreshDownload(t *testing.T) {
	s, actor, p, date := fixture(t)
	ctx := context.Background()
	fp := strings.Repeat("a", 64)
	queue(t, s, actor, date, fp)
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_bill_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='statement.download_completed' THEN RAISE EXCEPTION 'fixture rollback'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_bill_completion BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_bill_completion()`)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := registry(t, &billProvider{fingerprint: fp, download: func(payments.BillRequest) (payments.Bill, error) {
		calls++
		return payments.Bill{Provider: "wechat", Date: date, Kind: "payment", Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, date, p.ID)}, nil
	}})
	process(t, s, r)
	all, err := s.ListStatements(ctx)
	if err != nil || len(all) != 0 {
		t.Fatal("failed completion committed observations", all, err)
	}
	var financialCount int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM merchant_statement_financials`).Scan(&financialCount); err != nil || financialCount != 0 {
		t.Fatal("failed completion leaked financial snapshot", financialCount, err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "waiting" || jobs[0].LastOutcome != "storage_error" {
		t.Fatal(jobs, err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER fail_bill_completion ON audit_events; UPDATE bill_download_jobs SET next_attempt_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	process(t, s, r)
	all, err = s.ListStatements(ctx)
	if err != nil || len(all) != 1 || calls != 2 {
		t.Fatal("recovery lost or duplicated observations", all, calls, err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM merchant_statement_financials`).Scan(&financialCount); err != nil || financialCount != 1 {
		t.Fatal("financial snapshot lost or duplicated during recovery", financialCount, err)
	}
}
