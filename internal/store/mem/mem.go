// Package mem 提供内存实现，用于单元测试（不依赖数据库）。
package mem

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"lmgateway/internal/store"
)

// ---- JSONStore ----

type MemJSON struct {
	mu      sync.RWMutex
	docs    map[string]*store.JSONDoc
	version int64
	hub     *store.WatchHub
}

func NewJSON() store.JSONStore {
	return &MemJSON{
		docs: make(map[string]*store.JSONDoc),
		hub:  store.NewWatchHub(),
	}
}

func (m *MemJSON) Get(_ context.Context, key string) (*store.JSONDoc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.docs[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneDoc(d), nil
}

func (m *MemJSON) Put(_ context.Context, key string, data any) (*store.JSONDoc, error) {
	return m.update(key, data, 0)
}

func (m *MemJSON) Update(_ context.Context, key string, data any, expect int64) (*store.JSONDoc, error) {
	m.mu.RLock()
	cur, ok := m.docs[key]
	m.mu.RUnlock()
	if !ok || cur.Version != expect {
		return nil, store.ErrConflict
	}
	return m.update(key, data, expect)
}

func (m *MemJSON) update(key string, data any, expect int64) (*store.JSONDoc, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.docs[key]
	v := int64(1)
	if cur != nil {
		v = cur.Version + 1
	}
	d := &store.JSONDoc{Key: key, Data: raw, Version: v, UpdatedAt: time.Now().UTC()}
	m.docs[key] = d
	m.version++
	prefix := keyPrefix(key)
	m.hub.Notify(store.JSONChange{Prefix: prefix, Key: key})
	return cloneDoc(d), nil
}

func (m *MemJSON) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.docs[key]; !ok {
		return store.ErrNotFound
	}
	delete(m.docs, key)
	m.version++
	prefix := keyPrefix(key)
	m.hub.Notify(store.JSONChange{Prefix: prefix, Key: key})
	return nil
}

func (m *MemJSON) List(_ context.Context, prefix string) ([]*store.JSONDoc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*store.JSONDoc
	for k, d := range m.docs {
		if prefix == "" || len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, cloneDoc(d))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemJSON) Watch(_ context.Context, prefix string, fn func(store.JSONChange)) (func(), error) {
	return m.hub.Subscribe(prefix, fn)
}

func (m *MemJSON) Close() error { return nil }

func cloneDoc(d *store.JSONDoc) *store.JSONDoc {
	c := *d
	c.Data = append([]byte(nil), d.Data...)
	return &c
}

func keyPrefix(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i+1]
		}
	}
	return ""
}

// ---- TSStore（同步，供测试直接使用） ----

type TSBackend struct {
	mu      sync.Mutex
	records []*store.TSRecord
}

func NewTSBackend() *TSBackend { return &TSBackend{} }

// Records 直接访问底层记录（测试用）
func (m *TSBackend) Records() []*store.TSRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.records
}

func (m *TSBackend) AppendBatch(_ context.Context, recs []*store.TSRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, recs...)
	return nil
}

func (m *TSBackend) Query(_ context.Context, q store.TSQuery) ([]*store.TSRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*store.TSRecord
	for _, r := range m.records {
		if matchQuery(r, q) {
			out = append(out, r)
		}
	}
	if q.Order == "desc" {
		sort.Slice(out, func(i, j int) bool { return out[i].Ts.After(out[j].Ts) })
	} else {
		sort.Slice(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *TSBackend) Aggregate(_ context.Context, q store.TSAgg) ([]store.TSAggRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var recs []*store.TSRecord
	for _, r := range m.records {
		if store.AggMatch(r, q) {
			recs = append(recs, r)
		}
	}
	return store.AggregateRecords(recs, q), nil
}

func (m *TSBackend) DeleteByQuery(_ context.Context, stream string, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.records[:0]
	var n int64
	for _, r := range m.records {
		if r.Stream == stream && r.Ts.Before(before) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.records = kept
	return n, nil
}

func (m *TSBackend) Close() error { return nil }

func matchQuery(r *store.TSRecord, q store.TSQuery) bool {
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if !q.From.IsZero() && r.Ts.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && r.Ts.After(q.To) {
		return false
	}
	for k, v := range q.Tag {
		if r.Tags[k] != v {
			return false
		}
	}
	return true
}
