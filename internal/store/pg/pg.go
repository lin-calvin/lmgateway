package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"lmgateway/internal/migrate"
	"lmgateway/internal/store"
)

// Open 打开 Postgres：先用 stdlib 跑迁移，再用 pgxpool 建存储。
func Open(ctx context.Context, dsn string) (*DB, error) {
	// 迁移（database/sql + pgx stdlib 驱动）
	mdb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate.Apply(ctx, mdb, "postgres"); err != nil {
		mdb.Close()
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	mdb.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// DB 聚合 Postgres 的 JSONStore + TSBackend
type DB struct {
	pool *pgxpool.Pool
}

func (d *DB) Pool() *pgxpool.Pool { return d.pool }

func (d *DB) Close() { d.pool.Close() }

// ---- JSONStore ----

type JSONStore struct {
	pool     *pgxpool.Pool
	hub      *store.WatchHub
	watcher  *store.MetaWatcher
	listener *configListener
	knownVer int64
}

func (d *DB) NewJSONStore(ctx context.Context) (*JSONStore, error) {
	known, err := metaVersion(ctx, d.pool)
	if err != nil {
		return nil, err
	}
	j := &JSONStore{
		pool:     d.pool,
		hub:      store.NewWatchHub(),
		knownVer: known,
	}
	j.watcher = store.NewMetaWatcher(
		func(ctx context.Context) (int64, error) { return metaVersion(ctx, j.pool) },
		func(c store.JSONChange) { j.hub.Notify(c) },
		known,
	)
	j.watcher.Start(ctx, 5*time.Second)
	j.listener = newConfigListener(ctx, d.pool, func() { j.hub.Notify(store.JSONChange{Prefix: ""}) })
	return j, nil
}

func (j *JSONStore) Get(ctx context.Context, key string) (*store.JSONDoc, error) {
	row := j.pool.QueryRow(ctx,
		`SELECT ns_key, data, version, updated_at FROM store_json WHERE ns_key = $1`, key)
	return scanDoc(row.Scan)
}

func (j *JSONStore) Put(ctx context.Context, key string, data any) (*store.JSONDoc, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO store_json(ns_key, data, version, updated_at) VALUES ($1, $2::jsonb, 1, $3)
		 ON CONFLICT(ns_key) DO UPDATE SET data = $2::jsonb, version = store_json.version + 1, updated_at = $3`,
		key, raw, time.Now().UTC()); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	if err := bumpMetaAndNotify(ctx, tx); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	j.afterChange(ctx)
	return j.Get(ctx, key)
}

func (j *JSONStore) Update(ctx context.Context, key string, data any, expect int64) (*store.JSONDoc, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(ctx,
		`UPDATE store_json SET data = $2::jsonb, version = version + 1, updated_at = $3 WHERE ns_key = $1 AND version = $4`,
		key, raw, time.Now().UTC(), expect)
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	if res.RowsAffected() == 0 {
		tx.Rollback(ctx)
		return nil, store.ErrConflict
	}
	if err := bumpMetaAndNotify(ctx, tx); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	j.afterChange(ctx)
	return j.Get(ctx, key)
}

func (j *JSONStore) Delete(ctx context.Context, key string) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return err
	}
	res, err := tx.Exec(ctx, `DELETE FROM store_json WHERE ns_key = $1`, key)
	if err != nil {
		tx.Rollback(ctx)
		return err
	}
	if res.RowsAffected() == 0 {
		tx.Rollback(ctx)
		return store.ErrNotFound
	}
	if err := bumpMetaAndNotify(ctx, tx); err != nil {
		tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func (j *JSONStore) List(ctx context.Context, prefix string) ([]*store.JSONDoc, error) {
	rows, err := j.pool.Query(ctx,
		`SELECT ns_key, data, version, updated_at FROM store_json WHERE ns_key LIKE $1 ORDER BY ns_key`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.JSONDoc
	for rows.Next() {
		d, err := scanDoc(rows.Scan)
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

func (j *JSONStore) afterChange(ctx context.Context) {
	v, err := metaVersion(ctx, j.pool)
	if err == nil {
		j.knownVer = v
		j.watcher.MarkLocal(v)
	}
	j.hub.Notify(store.JSONChange{Prefix: "", Key: ""})
}

func (j *JSONStore) Close() error {
	j.watcher.Close()
	if j.listener != nil {
		j.listener.Close()
	}
	return nil
}

// ---- TSBackend ----

type TSBackend struct {
	pool *pgxpool.Pool
}

func (d *DB) NewTSBackend() *TSBackend { return &TSBackend{pool: d.pool} }

func (t *TSBackend) AppendBatch(ctx context.Context, recs []*store.TSRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return err
	}
	for _, r := range recs {
		tags, _ := json.Marshal(r.Tags)
		fields, _ := json.Marshal(r.Fields)
		if _, err := tx.Exec(ctx,
			`INSERT INTO store_ts(ts, stream, tags, fields) VALUES ($1, $2, $3::jsonb, $4::jsonb)`,
			r.Ts.UTC(), r.Stream, tags, fields); err != nil {
			tx.Rollback(ctx)
			return err
		}
	}
	return tx.Commit(ctx)
}

func (t *TSBackend) Query(ctx context.Context, q store.TSQuery) ([]*store.TSRecord, error) {
	query := `SELECT ts, stream, tags, fields FROM store_ts WHERE stream = $1`
	args := []any{q.Stream}
	n := 2
	if !q.From.IsZero() {
		query += fmt.Sprintf(` AND ts >= $%d`, n)
		args = append(args, q.From.UTC())
		n++
	}
	if !q.To.IsZero() {
		query += fmt.Sprintf(` AND ts <= $%d`, n)
		args = append(args, q.To.UTC())
		n++
	}
	keys := sortedKeys(q.Tag)
	for _, k := range keys {
		query += fmt.Sprintf(` AND tags->>$%d = $%d`, n, n+1)
		args = append(args, k, q.Tag[k])
		n += 2
	}
	query += ` ORDER BY ts ASC`
	if q.Order == "desc" {
		query = replaceOrder(query)
	}
	if q.Limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d`, n)
		args = append(args, q.Limit)
	}
	rows, err := t.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.TSRecord
	for rows.Next() {
		r, err := scanRecord(rows.Scan)
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
	res, err := t.pool.Exec(ctx, `DELETE FROM store_ts WHERE stream = $1 AND ts < $2`, stream, before.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

func (t *TSBackend) Close() error { return nil }

// ---- config listener（LISTEN/NOTIFY） ----

type configListener struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	notify func()
}

func newConfigListener(ctx context.Context, pool *pgxpool.Pool, notify func()) *configListener {
	lctx, cancel := context.WithCancel(ctx)
	l := &configListener{ctx: lctx, cancel: cancel, done: make(chan struct{}), notify: notify}
	go l.run(pool)
	return l
}

func (l *configListener) run(pool *pgxpool.Pool) {
	defer close(l.done)
	for {
		select {
		case <-l.ctx.Done():
			return
		default:
		}
		conn, err := pool.Acquire(l.ctx)
		if err != nil {
			if l.ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		pc := conn.Conn().PgConn()
		if _, err := pc.Exec(l.ctx, "LISTEN config_changed").ReadAll(); err != nil {
			conn.Release()
			time.Sleep(time.Second)
			continue
		}
		for {
			if err := pc.WaitForNotification(l.ctx); err != nil {
				break
			}
			l.notify()
		}
		conn.Release()
	}
}

func (l *configListener) Close() {
	l.cancel()
	<-l.done
}

// ---- helpers ----

func scanDoc(scan func(dest ...any) error) (*store.JSONDoc, error) {
	var key string
	var data []byte
	var version int64
	var updated time.Time
	if err := scan(&key, &data, &version, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &store.JSONDoc{Key: key, Data: data, Version: version, UpdatedAt: updated}, nil
}

func scanRecord(scan func(dest ...any) error) (*store.TSRecord, error) {
	var ts time.Time
	var stream string
	var tags, fields []byte
	if err := scan(&ts, &stream, &tags, &fields); err != nil {
		return nil, err
	}
	r := &store.TSRecord{Ts: ts, Stream: stream, Tags: map[string]string{}, Fields: map[string]any{}}
	json.Unmarshal(tags, &r.Tags)
	json.Unmarshal(fields, &r.Fields)
	return r, nil
}

func metaVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := pool.QueryRow(ctx, `SELECT version FROM store_json_meta WHERE id = 'global'`).Scan(&v)
	return v, err
}

func bumpMetaAndNotify(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `UPDATE store_json_meta SET version = version + 1 WHERE id = 'global'`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify('config_changed', '')`)
	return err
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
	for i := 0; i+len("ORDER BY ts ASC") <= len(q); i++ {
		if q[i:i+len("ORDER BY ts ASC")] == "ORDER BY ts ASC" {
			return q[:i] + "ORDER BY ts DESC" + q[i+len("ORDER BY ts ASC"):]
		}
	}
	return q
}
