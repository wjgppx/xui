package store_test

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func billStoreDate() string {
	return time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).AddDate(0, 0, -1).Format("2006-01-02")
}
func billStoreQueue(t *testing.T, s *store.Store, actor, provider, fp string) store.BillDownloadJob {
	t.Helper()
	j, err := s.RequestBillDownload(context.Background(), actor, provider, "payment", billStoreDate(), fp)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func billStoreResult(paymentID, provider string) statements.NativeResult {
	input := statements.Import{Provider: provider, Kind: "payment", Date: billStoreDate(), Coverage: "full", CSV: "payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at\n" + paymentID + ",transaction-" + paymentID + ",,,1900,CNY," + billStoreDate() + "T12:00:00+08:00\n"}
	format := "wechat_success_v1"
	if provider == "alipay" {
		input.Coverage = "partial"
		format = "alipay_trade_v1"
	}
	return statements.NativeResult{Input: input, Format: format, SourceDigest: strings.Repeat("b", 64), MerchantID: "fixture-merchant"}
}
func billStoreClaim(t *testing.T, s *store.Store) store.BillDownloadClaim {
	t.Helper()
	j, err := s.ClaimBillDownload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func billStoreNative(t *testing.T, paymentID string) statements.NativeImport {
	t.Helper()
	columns := "支付宝交易号,商户订单号,业务类型,商品名称,创建时间,完成时间,门店编号,门店名称,操作员,终端号,对方账户,订单金额（元）,商家实收（元）,支付宝红包（元）,集分宝（元）,支付宝优惠（元）,商家优惠（元）,券核销金额（元）,券名称,商家红包消费金额（元）,卡消费金额（元）,退款批次号/请求号,服务费（元）,分润（元）,备注"
	values := make([]string, 25)
	values[0], values[1], values[2], values[3] = "transaction-"+paymentID, paymentID, "交易", "private-fixture-product"
	values[4], values[5] = billStoreDate()+" 12:00:00", billStoreDate()+" 12:00:00"
	values[11], values[12] = "19.00", "18.00"
	var text strings.Builder
	w := csv.NewWriter(&text)
	_ = w.Write(strings.Split(columns, ","))
	_ = w.Write(values)
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	return statements.NativeImport{Provider: "alipay", Kind: "payment", Date: billStoreDate(), Coverage: "partial", Format: "alipay_trade_v1", ContentBase64: base64.StdEncoding.EncodeToString([]byte(text.String()))}
}
func TestBillDownloadQueueIsIdempotentAndBoundToMerchantIdentity(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	fp := strings.Repeat("a", 64)
	first := billStoreQueue(t, s, actor, "wechat", fp)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.RequestBillDownload(ctx, actor, "wechat", "payment", billStoreDate(), fp)
			if err != nil || j.ID != first.ID {
				t.Error("duplicate task", j.ID, err)
			}
		}()
	}
	wg.Wait()
	claim := billStoreClaim(t, s)
	if claim.MerchantFingerprint != fp || claim.ActorVersion != 0 {
		t.Fatal("merchant or authorization snapshot lost")
	}
	same := billStoreQueue(t, s, actor, "wechat", fp)
	if same.Status != "processing" {
		t.Fatal("active lease reset")
	}
	second := billStoreQueue(t, s, actor, "wechat", strings.Repeat("c", 64))
	if first.ID == second.ID {
		t.Fatal("new merchant inherited old task")
	}
	if _, err := s.RequestBillDownload(ctx, actor, "epay_alipay", "payment", billStoreDate(), fp); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal(err)
	}
}

func TestBillDownloadRequestCannotRenewAuthorityFromAnInvalidatedSession(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	fp := strings.Repeat("a", 64)
	if _, err := s.RequestBillDownload(ctx, actor, "wechat", "payment", billStoreDate(), fp, 0); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("invalidated request created new authority", err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 0 {
		t.Fatal("stale session left a job", err)
	}
	if _, err = s.RequestBillDownload(ctx, actor, "wechat", "payment", billStoreDate(), fp, 1); err != nil {
		t.Fatal("current session could not request bill", err)
	}
}
func TestNativeSourcesDeduplicateFactsAndRemainUnverifiedImmutableObservations(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, _, p := paymentFixture(t, s, "alipay")
	normal := billStoreResult(p.ID, "alipay")
	before, err := s.ImportStatement(ctx, actor, normal.Input)
	if err != nil {
		t.Fatal(err)
	}
	native := billStoreNative(t, p.ID)
	after, err := s.ImportNativeStatement(ctx, actor, native)
	if err != nil || after.Statement.ID != before.Statement.ID || len(after.Runs) != 1 || len(after.Sources) != 2 {
		t.Fatal("facts duplicated instead of provenance", after, err)
	}
	again, err := s.ImportNativeStatement(ctx, actor, native)
	if err != nil || len(again.Sources) != 2 {
		t.Fatal("source replay duplicated", err)
	}
	b, err := json.Marshal(again)
	if err != nil || strings.Contains(string(b), "private-fixture-product") {
		t.Fatal("raw source leaked", err)
	}
	for _, source := range again.Sources {
		if source.Integrity != "unverified_upload" || source.DownloadJobID != nil {
			t.Fatal("upload got trusted-source label")
		}
	}
	if _, err = s.DB.Exec(ctx, `UPDATE merchant_statement_sources SET integrity='alipay_signed_url' WHERE statement_id=$1`, again.Statement.ID); err == nil {
		t.Fatal("source evidence changed")
	}
	if _, err = s.DB.Exec(ctx, `DELETE FROM merchant_statement_sources WHERE statement_id=$1`, again.Statement.ID); err == nil {
		t.Fatal("source evidence deleted")
	}
	var grants int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM entitlement_periods`).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("native import granted rights", err)
	}
}
func TestBillDownloadLeaseRejectsStaleImportAndFinish(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	billStoreQueue(t, s, actor, "wechat", strings.Repeat("a", 64))
	old := billStoreClaim(t, s)
	if _, err := s.DB.Exec(ctx, `UPDATE bill_download_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	current := billStoreClaim(t, s)
	if current.ID != old.ID || current.ClaimToken == old.ClaimToken {
		t.Fatal("claim not recovered")
	}
	if _, err := s.CompleteBillDownload(ctx, old.ID, old.ClaimToken, billStoreResult("observed-payment", "wechat")); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal("stale claim imported", err)
	}
	if err := s.FinishBillDownload(ctx, old.ID, old.ClaimToken, "gateway_error"); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal(err)
	}
	items, err := s.ListStatements(ctx)
	if err != nil || len(items) != 0 {
		t.Fatal("stale import left evidence", err)
	}
}
func TestBillDownloadRoleAndSecurityVersionChangesRevokeImportAuthority(t *testing.T) {
	for _, change := range []string{"role", "version"} {
		t.Run(change, func(t *testing.T) {
			s := testutil.Database(t)
			ctx := context.Background()
			actor := actionAdmin(t, s)
			billStoreQueue(t, s, actor, "wechat", strings.Repeat("a", 64))
			claim := billStoreClaim(t, s)
			query := `UPDATE users SET security_version=security_version+1 WHERE id=$1`
			if change == "role" {
				query = `UPDATE users SET role='user' WHERE id=$1`
			}
			if _, err := s.DB.Exec(ctx, query, actor); err != nil {
				t.Fatal(err)
			}
			if allowed, err := s.BillDownloadAllowed(ctx, claim.ID, claim.ClaimToken); allowed || !errors.Is(err, store.ErrRoleForbidden) {
				t.Fatal("obsolete authority permitted", err)
			}
			if _, err := s.CompleteBillDownload(ctx, claim.ID, claim.ClaimToken, billStoreResult("observed-payment", "wechat")); !errors.Is(err, store.ErrRoleForbidden) {
				t.Fatal("obsolete authority imported", err)
			}
		})
	}
}
func TestBillDownloadRetriesHorizonAndParsingFailureRequireManualReview(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	fp := strings.Repeat("a", 64)
	job := billStoreQueue(t, s, actor, "wechat", fp)
	for i := 0; i < 8; i++ {
		claim := billStoreClaim(t, s)
		if err := s.FinishBillDownload(ctx, claim.ID, claim.ClaimToken, "gateway_error"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(ctx, `UPDATE bill_download_jobs SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || jobs[0].Status != "manual_required" || jobs[0].Attempts != 8 {
		t.Fatal(jobs, err)
	}
	again := billStoreQueue(t, s, actor, "wechat", fp)
	if again.ID != job.ID || again.Attempts != 0 || again.Status != "queued" {
		t.Fatal("retry replaced identity")
	}
	claim := billStoreClaim(t, s)
	if err = s.FinishBillDownload(ctx, claim.ID, claim.ClaimToken, "parse_error"); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.ListBillDownloads(ctx)
	if err != nil || jobs[0].Status != "manual_required" {
		t.Fatal("incompatible file repeatedly retried", jobs, err)
	}
	billStoreQueue(t, s, actor, "wechat", fp)
	if _, err = s.DB.Exec(ctx, `UPDATE bill_download_jobs SET horizon_at=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	claim = billStoreClaim(t, s)
	if allowed, err := s.BillDownloadAllowed(ctx, claim.ID, claim.ClaimToken); err != nil || allowed {
		t.Fatal("expired task allowed outbound request", err)
	}
}
func TestBillDownloadCompletionRollsBackSourceReportAndTaskTogether(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	fp := strings.Repeat("a", 64)
	job := billStoreQueue(t, s, actor, "wechat", fp)
	claim := billStoreClaim(t, s)
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_bill_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='statement.download_completed' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_bill_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_bill_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	result := billStoreResult("observed-payment", "wechat")
	if _, err = s.CompleteBillDownload(ctx, claim.ID, claim.ClaimToken, result); err == nil {
		t.Fatal("failed audit committed import")
	}
	all, err := s.ListStatements(ctx)
	if err != nil || len(all) != 0 {
		t.Fatal("partial evidence after rollback", err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER fail_bill_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	detail, err := s.CompleteBillDownload(ctx, claim.ID, claim.ClaimToken, result)
	if err != nil || len(detail.Sources) != 1 || detail.Sources[0].DownloadJobID == nil || *detail.Sources[0].DownloadJobID != job.ID || detail.Sources[0].Integrity != "wechat_signed_sha1" {
		t.Fatal(detail, err)
	}
	same := billStoreQueue(t, s, actor, "wechat", fp)
	if same.Status != "succeeded" || same.StatementID == nil || *same.StatementID != detail.Statement.ID {
		t.Fatal("completed task repeated")
	}
}
func TestBillDownloadRefusesResultScopeOrProviderMismatch(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	billStoreQueue(t, s, actor, "wechat", strings.Repeat("a", 64))
	claim := billStoreClaim(t, s)
	for _, change := range []string{"date", "provider", "kind", "format", "coverage", "digest"} {
		result := billStoreResult("observed-payment", "wechat")
		switch change {
		case "date":
			result.Input.Date = "2000-01-01"
		case "provider":
			result.Input.Provider = "alipay"
		case "kind":
			result.Input.Kind = "refund"
		case "format":
			result.Format = "alipay_trade_v1"
		case "coverage":
			result.Input.Coverage = "partial"
		case "digest":
			result.SourceDigest = "bad"
		}
		if _, err := s.CompleteBillDownload(ctx, claim.ID, claim.ClaimToken, result); err == nil {
			t.Fatal("wrong result scope imported", change)
		}
	}
	all, err := s.ListStatements(ctx)
	if err != nil || len(all) != 0 {
		t.Fatal(err)
	}
}
func TestBillSourcesMigrationBackfillsExistingNormalizedEvidence(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	// Rewind only the removed modules; later independent ledgers stay applied.
	for _, query := range []string{`DROP TABLE merchant_statement_financial_runs`, `DROP TABLE merchant_statement_financials`, `ALTER TABLE bill_download_jobs DROP CONSTRAINT bill_download_jobs_schedule_id_fkey`, `DROP TABLE bill_download_schedules`, `DROP TABLE merchant_statement_sources`, `DROP TABLE bill_download_jobs`, `DELETE FROM schema_migrations WHERE version BETWEEN 12 AND 14`} {
		if _, err := s.DB.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	id := store.ID()
	digest := strings.Repeat("d", 64)
	if _, err := s.DB.Exec(ctx, `INSERT INTO merchant_statements(id,provider,kind,business_date,coverage,digest,imported_by,row_count,total_minor) VALUES($1,'wechat','payment',$2::date,'partial',$3,$4,0,0)`, id, billStoreDate(), digest, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO merchant_statement_runs(id,statement_id,revision,matched_count,difference_count,local_candidate_count,as_of,created_by) VALUES($1,$2,1,0,0,0,clock_timestamp(),$3)`, store.ID(), id, actor); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Statement(ctx, id)
	if err != nil || len(detail.Sources) != 1 || detail.Sources[0].Format != "normalized_csv_v1" || detail.Sources[0].Digest != digest || detail.Sources[0].Integrity != "unverified_upload" {
		t.Fatal("v11 evidence did not survive migration", detail, err)
	}
}
