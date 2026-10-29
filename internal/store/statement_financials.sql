CREATE TABLE merchant_statement_financials (
 source_id text PRIMARY KEY REFERENCES merchant_statement_sources(id),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 document jsonb NOT NULL CHECK(jsonb_typeof(document)='object' AND document->>'version'='native-financials-v1' AND jsonb_typeof(document->'rows')='array' AND jsonb_array_length(document->'rows')<=2000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE merchant_statement_financial_runs (
 id text PRIMARY KEY,
 source_id text NOT NULL REFERENCES merchant_statement_financials(source_id),
 revision integer NOT NULL CHECK(revision>0),
 reference_digest text NOT NULL CHECK(reference_digest ~ '^[0-9a-f]{64}$'),
 report jsonb NOT NULL CHECK(jsonb_typeof(report)='object' AND jsonb_typeof(report->'rows')='array' AND jsonb_array_length(report->'rows')<=4000),
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(source_id,revision),
 UNIQUE(source_id,reference_digest)
);
CREATE INDEX merchant_statement_financial_runs_source ON merchant_statement_financial_runs(source_id,revision DESC);
CREATE TRIGGER merchant_statement_financials_no_update BEFORE UPDATE OR DELETE ON merchant_statement_financials
 FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
CREATE TRIGGER merchant_statement_financial_runs_no_update BEFORE UPDATE OR DELETE ON merchant_statement_financial_runs
 FOR EACH ROW EXECUTE FUNCTION merchant_statement_immutable();
