package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"lmgateway/internal/migrate"
	"lmgateway/internal/store"
)

const metaVersionSQL = `SELECT version FROM store_json_meta WHERE id = 'global'`

// Open 打开 SQLite（自动 WAL + busy_timeout + 迁移）。dsn 为文件路径。
func Open(ctx context.Context, dsn string) (*DB, error) {
	uri := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dsn)
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：单连接避免 SQLITE_BUSY
	db.SetMaxOpenConns(1)
	if err := migrate.Apply(ctx, db, "sqlite"); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

// DB 聚合 SQLite 的 JSONStore + TSBackend
type DB struct {
	db *sql.DB
}

func (d *DB) SQLDB() *sql.DB { return d.db }

func (d *DB) Close() error { return d.db.Close() }

// ---- JSONStore ----

type JSONStore struct {
	db       *sql.DB
	hub      *store.WatchHub
	watcher  *store.MetaWatcher
	knownVer int64
}

func (d *DB) NewJSONStore(ctx context.Context) (*JSONStore, error) {
	known, err := metaVersion(ctx, d.db)
	if err != nil {
		return nil, err
	}
	j := &JSONStore{
		db:       d.db,
		hub:      store.NewWatchHub(),
		knownVer: known,
	}
	j.watcher = store.NewMetaWatcher(
		func(ctx context.Context) (int64, error) { return metaVersion(ctx, j.db) },
		func(c store.JSONChange) { j.hub.Notify(c) },
		known,
	)
	j.watcher.Start(ctx, 3*time.Second)
	return j, nil
}

func (j *JSONStore) Get(_ context.Context, key string) (*store.JSONDoc, error) {
	row := j.db.QueryRow(`SELECT ns_key, data, version, updated_at FROM store_json WHERE ns_key = ?`, key)
	return scanDoc(row)
}

func (j *JSONStore) Put(ctx context.Context, key string, data any) (*store.JSONDoc, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	now := tsText(time.Now())
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO store_json(ns_key, data, version, updated_at) VALUES (?, ?, 1, ?)
		ON CONFLICT(ns_key) DO UPDATE SET data = excluded.data, version = store_json.version + 1, updated_at = excluded.updated_at`,
		key, string(raw), now); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := bumpMeta(ctx, tx); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.afterChange(ctx)
	doc, err := j.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return doc, nil
}

func (j *JSONStore) Update(ctx context.Context, key string, data any, expect int64) (*store.JSONDoc, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	now := tsText(time.Now())
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE store_json SET data = ?, version = version + 1, updated_at = ? WHERE ns_key = ? AND version = ?`,
		string(raw), now, key, expect)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		tx.Rollback()
		return nil, store.ErrConflict
	}
	if err := bumpMeta(ctx, tx); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.afterChange(ctx)
	return j.Get(ctx, key)
}

func (j *JSONStore) Delete(ctx context.Context, key string) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM store_json WHERE ns_key = ?`, key)
	if err != nil {
		tx.Rollback()
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		tx.Rollback()
		return store.ErrNotFound
	}
	if err := bumpMeta(ctx, tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	j.afterChange(ctx)
	return nil
}

func (j *JSONStore) List(_ context.Context, prefix string) ([]*store.JSONDoc, error) {
	rows, err := j.db.Query(`SELECT ns_key, data, version, updated_at FROM store_json WHERE ns_key LIKE ? ORDER BY ns_key`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.JSONDoc
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (j *JSONStore) Watch(_ context.Context, prefix string, fn func(store.JSONChange)) (func(), error) {
	return j.hub.Subscribe(prefix, fn)
}

// afterChange 本地写后：同步已知版本 + 本地通知（避免 poller 重复触发）
func (j *JSONStore) afterChange(ctx context.Context) {
	v, err := metaVersion(ctx, j.db)
	if err == nil {
		j.knownVer = v
		j.watcher.MarkLocal(v)
	}
	j.hub.Notify(store.JSONChange{Prefix: "", Key: ""})
}

func (j *JSONStore) Close() error {
	j.watcher.Close()
	return nil
}

// ---- TSBackend ----

type TSBackend struct {
	db *sql.DB
}

func (d *DB) NewTSBackend() *TSBackend { return &TSBackend{db: d.db} }

func (t *TSBackend) AppendBatch(ctx context.Context, recs []*store.TSRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO store_ts(ts, stream, tags, fields) VALUES (?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range recs {
		tags, _ := json.Marshal(r.Tags)
		fields, _ := json.Marshal(r.Fields)
		if _, err := stmt.Exec(tsText(r.Ts), r.Stream, string(tags), string(fields)); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (t *TSBackend) Query(ctx context.Context, q store.TSQuery) ([]*store.TSRecord, error) {
	query := `SELECT ts, stream, tags, fields FROM store_ts WHERE stream = ?`
	args := []any{q.Stream}
	if !q.From.IsZero() {
		query += ` AND ts >= ?`
		args = append(args, tsText(q.From))
	}
	if !q.To.IsZero() {
		query += ` AND ts <= ?`
		args = append(args, tsText(q.To))
	}
	keys := sortedKeys(q.Tag)
	for _, k := range keys {
		query += fmt.Sprintf(` AND json_extract(tags, '$.%s') = ?`, k)
		args = append(args, q.Tag[k])
	}
	query += ` ORDER BY ts ASC`
	if q.Order == "desc" {
		query = replaceOrder(query)
	}
	if q.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, q.Limit)
	}
	rows, err := t.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.TSRecord
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *TSBackend) Aggregate(ctx context.Context, q store.TSAgg) ([]store.TSAggRow, error) {
	recs, err := t.Query(ctx, store.TSQuery{
		Stream: q.Stream, From: q.From, To: q.To, Tag: q.Where,
	})
	if err != nil {
		return nil, err
	}
	return store.AggregateRecords(recs, q), nil
}

func (t *TSBackend) DeleteByQuery(ctx context.Context, stream string, before time.Time) (int64, error) {
	res, err := t.db.ExecContext(ctx, `DELETE FROM store_ts WHERE stream = ? AND ts < ?`, stream, tsText(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (t *TSBackend) Close() error { return nil }

// ---- helpers ----

func scanDoc(sc interface{ Scan(...any) error }) (*store.JSONDoc, error) {
	var key, data, updated string
	var version int64
	if err := sc.Scan(&key, &data, &version, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &store.JSONDoc{
		Key:       key,
		Data:      json.RawMessage(data),
		Version:   version,
		UpdatedAt: parseTS(updated),
	}, nil
}

func scanRecord(sc interface{ Scan(...any) error }) (*store.TSRecord, error) {
	var ts, stream, tags, fields string
	if err := sc.Scan(&ts, &stream, &tags, &fields); err != nil {
		return nil, err
	}
	r := &store.TSRecord{Ts: parseTS(ts), Stream: stream, Tags: map[string]string{}, Fields: map[string]any{}}
	json.Unmarshal([]byte(tags), &r.Tags)
	json.Unmarshal([]byte(fields), &r.Fields)
	return r, nil
}

func metaVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, metaVersionSQL).Scan(&v)
	return v, err
}

func bumpMeta(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE store_json_meta SET version = version + 1 WHERE id = 'global'`)
	return err
}

func tsText(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		log.Printf("[sqlite] parse ts %q: %v", s, err)
	}
	return t
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func replaceOrder(q string) string {
	// 简单替换首个 "ORDER BY ts ASC"
	for i := 0; i+len("ORDER BY ts ASC") <= len(q); i++ {
		if q[i:i+len("ORDER BY ts ASC")] == "ORDER BY ts ASC" {
			return q[:i] + "ORDER BY ts DESC" + q[i+len("ORDER BY ts ASC"):]
		}
	}
	return q
}
