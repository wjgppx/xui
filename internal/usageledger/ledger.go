// Package usageledger stores cumulative traffic cursors and immutable delivery
// batches in one local SQLite transaction. No remote acknowledgement can advance
// or discard a cursor independently of its already persisted batch.
package usageledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/wjgppx/xui/internal/agentwire"
)

const MaxPendingBatches = 10000
const MaxPendingBytes = 64 * 1024 * 1024

// Small chunks stay below the control API's 64 KiB request body limit, including
// worst-case identifiers. All chunks from a sample commit atomically.
const maxItemsPerBatch = 100

var ErrFull = errors.New("usage outbox capacity exhausted; stop traffic until delivery recovers")
var ErrRegression = errors.New("cumulative traffic counter regressed within one core instance")
var ErrAck = errors.New("usage acknowledgement digest does not match durable batch")
var ErrOverflow = errors.New("usage byte or sequence overflow")
var ErrIdentity = errors.New("usage ledger belongs to a different or unknown node identity")

type Counter struct {
	BindingID, UserID, EntitlementID string
	// Nil means absent from this query: retain a previous direction or use zero
	// for a newly observed subject. An explicit zero is a real counter value.
	Uplink, Downlink *int64
}
type Ledger struct {
	mu                   sync.Mutex
	db                   *sql.DB
	lock                 *os.File
	closed               bool
	scopeBound           bool
	maxBatches, maxBytes int64
}

func Open(path string) (*Ledger, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(absolute)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("usage ledger parent directory must be private (0700)")
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err = file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err = os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("usage ledger must be a private regular file (0600)")
	}
	lockPath := absolute + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err = os.Lstat(lockPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, errors.New("invalid usage ledger lock file")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("usage ledger already owned by another Agent")
	}
	release := func() { syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); lock.Close() }
	query := url.Values{"_busy_timeout": {"5000"}, "_journal_mode": {"WAL"}, "_synchronous": {"FULL"}, "_foreign_keys": {"on"}}
	dsn := (&url.URL{Scheme: "file", Path: absolute, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		release()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err = db.Exec(`
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS instances (
  id TEXT PRIMARY KEY,started_at INTEGER NOT NULL,last_sample INTEGER NOT NULL,
  sequence INTEGER NOT NULL DEFAULT 0 CHECK(sequence>=0)
 );
 CREATE TABLE IF NOT EXISTS cursors (
  instance_id TEXT NOT NULL REFERENCES instances(id),binding_id TEXT NOT NULL,
  user_id TEXT NOT NULL,entitlement_id TEXT NOT NULL,
  uplink INTEGER NOT NULL CHECK(uplink>=0),downlink INTEGER NOT NULL CHECK(downlink>=0),
  PRIMARY KEY(instance_id,binding_id,user_id,entitlement_id)
 );
 CREATE TABLE IF NOT EXISTS outbox (
  position INTEGER PRIMARY KEY AUTOINCREMENT,batch_id TEXT NOT NULL UNIQUE,
  instance_id TEXT NOT NULL REFERENCES instances(id),sequence INTEGER NOT NULL,
  digest TEXT NOT NULL,payload BLOB NOT NULL,
  UNIQUE(instance_id,sequence)
 );`)
	if err != nil {
		db.Close()
		release()
		return nil, err
	}
	// SQLite FULL synchronizes database/WAL contents; persist the directory
	// entries created during first initialization as well.
	directory, err := os.Open(parent)
	if err == nil {
		err = directory.Sync()
		closeErr := directory.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		db.Close()
		release()
		return nil, err
	}
	var scopeHash string
	err = db.QueryRow(`SELECT value FROM metadata WHERE key='identity_scope'`).Scan(&scopeHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		release()
		return nil, err
	}
	bound := err == nil
	if bound {
		raw, decodeErr := hex.DecodeString(scopeHash)
		if decodeErr != nil || len(raw) != sha256.Size {
			db.Close()
			release()
			return nil, ErrIdentity
		}
	}
	return &Ledger{db: db, lock: lock, maxBatches: MaxPendingBatches, maxBytes: MaxPendingBytes, scopeBound: bound}, nil
}

// BindIdentity fixes the server-side deduplication namespace before any sample
// can be recorded. The caller passes controller origin + NUL + stable node ID.
// Reusing a credential for the same node is safe; a different node/controller
// cannot reinterpret old pending batches. Only the scope digest is persisted.
func (l *Ledger) BindIdentity(scope string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("usage ledger is closed")
	}
	if scope == "" || len(scope) > 4096 {
		return ErrIdentity
	}
	sum := sha256.Sum256([]byte(scope))
	digest := hex.EncodeToString(sum[:])
	var existing string
	err := l.db.QueryRow(`SELECT value FROM metadata WHERE key='identity_scope'`).Scan(&existing)
	if err == nil {
		if existing != digest {
			return ErrIdentity
		}
		l.scopeBound = true
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var count int64
	if err = l.db.QueryRow(`SELECT (SELECT count(*) FROM instances)+(SELECT count(*) FROM outbox)`).Scan(&count); err != nil {
		return err
	}
	// An old unbound database is not evidence that its traffic belongs to this
	// node. Preserve it for reconciliation instead of assigning ownership.
	if count != 0 {
		return ErrIdentity
	}
	if _, err = l.db.Exec(`INSERT INTO metadata(key,value) VALUES('identity_scope',?)`, digest); err != nil {
		return err
	}
	l.scopeBound = true
	return nil
}

func counterKey(c Counter) string { return c.BindingID + "\x00" + c.UserID + "\x00" + c.EntitlementID }
func validID(value string, limit int) bool {
	return value != "" && len(value) <= limit && !strings.ContainsRune(value, 0)
}

// Sample persists a non-resetting counter observation. The first observation of
// a fresh process counts from zero, including bytes transferred before sampling.
// startedAt must remain the true, fixed process start time across observations.
func (l *Ledger) Sample(instance string, startedAt, at time.Time, counters []Counter) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("usage ledger is closed")
	}
	if !validID(instance, 64) || startedAt.IsZero() || !at.After(startedAt) {
		return errors.New("invalid usage sampling interval")
	}
	if !l.scopeBound {
		return ErrIdentity
	}
	startedAt, at = startedAt.UTC(), at.UTC()
	ordered := append([]Counter(nil), counters...)
	sort.Slice(ordered, func(i, j int) bool { return counterKey(ordered[i]) < counterKey(ordered[j]) })
	for i, c := range ordered {
		if !validID(c.BindingID, 48) || !validID(c.UserID, 64) || !validID(c.EntitlementID, 64) || (c.Uplink == nil && c.Downlink == nil) || (c.Uplink != nil && *c.Uplink < 0) || (c.Downlink != nil && *c.Downlink < 0) || (i > 0 && counterKey(c) == counterKey(ordered[i-1])) {
			return errors.New("invalid or duplicate usage counter")
		}
	}
	tx, err := l.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var start, last, sequence int64
	err = tx.QueryRow(`SELECT started_at,last_sample,sequence FROM instances WHERE id=?`, instance).Scan(&start, &last, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		start, last = startedAt.UnixNano(), startedAt.UnixNano()
		_, err = tx.Exec(`INSERT INTO instances(id,started_at,last_sample,sequence) VALUES(?,?,?,0)`, instance, start, last)
	}
	if err != nil {
		return err
	}
	if start != startedAt.UnixNano() || at.UnixNano() <= last || at.Sub(time.Unix(0, last)) > 2*time.Minute {
		return errors.New("invalid or nonmonotonic usage sampling interval")
	}
	items := []agentwire.UsageItem{}
	var total int64
	for _, c := range ordered {
		var oldUp, oldDown int64
		err = tx.QueryRow(`SELECT uplink,downlink FROM cursors WHERE instance_id=? AND binding_id=? AND user_id=? AND entitlement_id=?`, instance, c.BindingID, c.UserID, c.EntitlementID).Scan(&oldUp, &oldDown)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		up, down := oldUp, oldDown
		if c.Uplink != nil {
			up = *c.Uplink
		}
		if c.Downlink != nil {
			down = *c.Downlink
		}
		if up < oldUp || down < oldDown {
			return ErrRegression
		}
		deltaUp, deltaDown := up-oldUp, down-oldDown
		if deltaUp > math.MaxInt64-deltaDown || total > math.MaxInt64-deltaUp-deltaDown {
			return ErrOverflow
		}
		total += deltaUp + deltaDown
		if deltaUp+deltaDown > 0 {
			items = append(items, agentwire.UsageItem{BindingID: c.BindingID, UserID: c.UserID, EntitlementID: c.EntitlementID, UplinkBytes: deltaUp, DownlinkBytes: deltaDown})
		}
		if _, err = tx.Exec(`INSERT INTO cursors(instance_id,binding_id,user_id,entitlement_id,uplink,downlink) VALUES(?,?,?,?,?,?) ON CONFLICT(instance_id,binding_id,user_id,entitlement_id) DO UPDATE SET uplink=excluded.uplink,downlink=excluded.downlink`, instance, c.BindingID, c.UserID, c.EntitlementID, up, down); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		var count, size int64
		if err = tx.QueryRow(`SELECT count(*),COALESCE(sum(length(payload)),0) FROM outbox`).Scan(&count, &size); err != nil {
			return err
		}
		chunks := (len(items) + maxItemsPerBatch - 1) / maxItemsPerBatch
		if sequence > math.MaxInt64-int64(chunks) {
			return ErrOverflow
		}
		if count+int64(chunks) > l.maxBatches {
			return ErrFull
		}
		for offset := 0; offset < len(items); offset += maxItemsPerBatch {
			end := offset + maxItemsPerBatch
			if end > len(items) {
				end = len(items)
			}
			id := make([]byte, 16)
			if _, err = rand.Read(id); err != nil {
				return err
			}
			sequence++
			batch := agentwire.UsageBatch{BatchID: hex.EncodeToString(id), CoreInstanceID: instance, Sequence: sequence, StartedAt: time.Unix(0, last).UTC(), EndedAt: at, Items: items[offset:end]}
			payload, err := json.Marshal(batch)
			if err != nil {
				return err
			}
			digest, err := agentwire.UsageDigest(batch)
			if err != nil {
				return err
			}
			size += int64(len(payload))
			if size > l.maxBytes {
				return ErrFull
			}
			if _, err = tx.Exec(`INSERT INTO outbox(batch_id,instance_id,sequence,digest,payload) VALUES(?,?,?,?,?)`, batch.BatchID, instance, sequence, digest, payload); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(`UPDATE instances SET last_sample=?,sequence=? WHERE id=?`, at.UnixNano(), sequence, instance); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) Pending(limit int) ([]agentwire.UsageBatch, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("usage ledger is closed")
	}
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid pending batch limit")
	}
	rows, err := l.db.Query(`SELECT payload,digest FROM outbox ORDER BY position LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []agentwire.UsageBatch{}
	for rows.Next() {
		var payload []byte
		var stored string
		if err = rows.Scan(&payload, &stored); err != nil {
			return nil, err
		}
		var batch agentwire.UsageBatch
		if err = json.Unmarshal(payload, &batch); err != nil {
			return nil, errors.New("corrupt durable usage batch")
		}
		digest, err := agentwire.UsageDigest(batch)
		if err != nil || digest != stored {
			return nil, errors.New("corrupt durable usage batch")
		}
		result = append(result, batch)
	}
	return result, rows.Err()
}

// Ack deletes only a batch whose persisted content matches the server digest.
// Repeating an acknowledgement after deletion is harmless.
func (l *Ledger) Ack(batchID, digest string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("usage ledger is closed")
	}
	var stored string
	err := l.db.QueryRow(`SELECT digest FROM outbox WHERE batch_id=?`, batchID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if digest != stored {
		return ErrAck
	}
	_, err = l.db.Exec(`DELETE FROM outbox WHERE batch_id=? AND digest=?`, batchID, digest)
	return err
}
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := l.db.Close()
	if unlockErr := syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN); err == nil {
		err = unlockErr
	}
	if closeErr := l.lock.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Stats reports local persisted outbox size without exposing payload credentials.
func (l *Ledger) Stats() (batches, bytes int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, 0, errors.New("usage ledger is closed")
	}
	err = l.db.QueryRow(`SELECT count(*),COALESCE(sum(length(payload)),0) FROM outbox`).Scan(&batches, &bytes)
	return
}
