package httpapi

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/agentwire"
)

var usageIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validateUsage(batch agentwire.UsageBatch) bool {
	if !usageIDPattern.MatchString(batch.BatchID) || !usageIDPattern.MatchString(batch.CoreInstanceID) || batch.Sequence < 1 || batch.StartedAt.IsZero() || batch.StartedAt.Year() < 1 || batch.EndedAt.Year() < 1 || !batch.EndedAt.After(batch.StartedAt) || batch.EndedAt.Sub(batch.StartedAt) > 2*time.Minute || batch.EndedAt.After(time.Now().Add(30*time.Second)) || len(batch.Items) < 1 || len(batch.Items) > 1000 {
		return false
	}
	seen := map[string]bool{}
	var total int64
	for _, item := range batch.Items {
		if !agentwire.NamePattern.MatchString(item.BindingID) || !usageIDPattern.MatchString(item.UserID) || !usageIDPattern.MatchString(item.EntitlementID) || item.UplinkBytes < 0 || item.DownlinkBytes < 0 || (item.UplinkBytes == 0 && item.DownlinkBytes == 0) || item.UplinkBytes > math.MaxInt64-item.DownlinkBytes {
			return false
		}
		bytes := item.UplinkBytes + item.DownlinkBytes
		if bytes > math.MaxInt64-total {
			return false
		}
		total += bytes
		key := item.BindingID + "/" + item.UserID + "/" + item.EntitlementID
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

// usage atomically commits immutable evidence, its attribution ledger and exact
// byte debits. Node authorization is rechecked under lock to serialize revoke.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	nodeID, _, ok := s.agentIdentity(w, r)
	if !ok {
		return
	}
	var batch agentwire.UsageBatch
	if !decode(w, r, &batch) {
		return
	}
	if !validateUsage(batch) {
		fail(w, 400, "流量批次格式、字节数或采样时间不正确")
		return
	}
	digest, err := agentwire.UsageDigest(batch)
	if err != nil {
		fail(w, 400, "流量批次无法编码")
		return
	}
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var locked string
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	err = tx.QueryRow(r.Context(), `SELECT id FROM nodes WHERE id=$1 AND agent_token_hash=$2 AND revoked_at IS NULL FOR UPDATE`, nodeID, hash(token)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "节点身份失效")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	var previous string
	err = tx.QueryRow(r.Context(), `SELECT digest FROM usage_batches WHERE node_id=$1 AND batch_id=$2`, nodeID, batch.BatchID).Scan(&previous)
	if err == nil {
		if previous != digest {
			fail(w, 409, "批次标识已被用于不同内容")
			return
		}
		reply(w, 200, agentwire.UsageAck{BatchID: batch.BatchID, Digest: digest})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, err)
		return
	}
	var sequenceExists bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM usage_batches WHERE node_id=$1 AND core_instance_id=$2 AND sequence=$3)`, nodeID, batch.CoreInstanceID, batch.Sequence).Scan(&sequenceExists)
	if err != nil {
		serverError(w, err)
		return
	}
	if sequenceExists {
		fail(w, 409, "内核采样序号已被其他批次使用")
		return
	}
	// Validate all items before locking/updating any balance. Historical periods
	// stay eligible for late delivery if this node was actually granted them.
	sums := map[string]int64{}
	owners := map[string]string{}
	for _, item := range batch.Items {
		var issued bool
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM node_usage_authorizations a JOIN entitlement_periods e ON e.id=a.entitlement_period_id AND e.user_id=a.user_id WHERE a.node_id=$1 AND a.binding_id=$2 AND a.user_id=$3 AND a.entitlement_period_id=$4 AND a.authorized_from<=$5::timestamptz+interval '30 seconds' AND a.authorized_until>=$6::timestamptz-interval '30 seconds')`, nodeID, item.BindingID, item.UserID, item.EntitlementID, batch.StartedAt, batch.EndedAt).Scan(&issued)
		if err != nil {
			serverError(w, err)
			return
		}
		if !issued {
			fail(w, 403, "流量归属未获得该节点的历史授权")
			return
		}
		sums[item.EntitlementID] += item.UplinkBytes + item.DownlinkBytes
		owners[item.EntitlementID] = item.UserID
	}
	periods := make([]string, 0, len(sums))
	for id := range sums {
		periods = append(periods, id)
	}
	sort.Strings(periods)
	for _, id := range periods {
		var used int64
		err = tx.QueryRow(r.Context(), `SELECT used_bytes FROM entitlement_periods WHERE id=$1 AND user_id=$2 FOR NO KEY UPDATE`, id, owners[id]).Scan(&used)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 403, "流量权益归属不正确")
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		if sums[id] > math.MaxInt64-used {
			fail(w, 409, "流量累计超出可表示范围")
			return
		}
	}
	payload, _ := json.Marshal(batch)
	_, err = tx.Exec(r.Context(), `INSERT INTO usage_batches(node_id,batch_id,core_instance_id,sequence,started_at,ended_at,digest,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, nodeID, batch.BatchID, batch.CoreInstanceID, batch.Sequence, batch.StartedAt, batch.EndedAt, digest, payload)
	if err != nil {
		serverError(w, err)
		return
	}
	for i, item := range batch.Items {
		_, err = tx.Exec(r.Context(), `INSERT INTO usage_items(node_id,batch_id,item_index,binding_id,user_id,entitlement_period_id,uplink_bytes,downlink_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, nodeID, batch.BatchID, i, item.BindingID, item.UserID, item.EntitlementID, item.UplinkBytes, item.DownlinkBytes)
		if err != nil {
			serverError(w, err)
			return
		}
	}
	for _, id := range periods {
		_, err = tx.Exec(r.Context(), `UPDATE entitlement_periods SET used_bytes=used_bytes+$1 WHERE id=$2`, sums[id], id)
		if err != nil {
			serverError(w, err)
			return
		}
	}
	if err = tx.Commit(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, agentwire.UsageAck{BatchID: batch.BatchID, Digest: digest})
}
