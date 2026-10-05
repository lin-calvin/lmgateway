// Package store 提供两个存储风格给上层 middleware：
//
//	JSONStore — 版本化 KV/文档（配置等灵活数据）
//	TSStore   — 追加 + 时间范围查询 + 聚合（spend / 指标 / 事件）
//
// 底层实现可替换：Postgres（生产）、SQLite（开发/单测）、内存（单测）。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// 内置 stream 名。写方与读方共用常量，避免字符串拼写漂移。
const (
	// StreamSpend 原始 spend 流水（有 usage 的成功请求）。
	StreamSpend = "spend"
	// StreamSpendDaily rollup 产出的日汇总。
	StreamSpendDaily = "spend_daily"
	// StreamRequestError 数据面失败事件（配额拒绝、上游失败等）。
	// 告警引擎据此统计配额打满与上游大面积失败。
	StreamRequestError = "request_error"
)

// ---- JSON 风格 ----

// JSONDoc 一个文档
type JSONDoc struct {
	Key       string
	Data      json.RawMessage
	Version   int64
	UpdatedAt time.Time
}

// JSONChange 配置变更通知（用于热更）
type JSONChange struct {
	Prefix string // 变更前缀（如 "provider/"），空 = 全局
	Key    string
}

// JSONStore 版本化 KV/文档存储。key 为复合名，如 "provider/openai"、"rule/1"。
type JSONStore interface {
	Get(ctx context.Context, key string) (*JSONDoc, error)
	Put(ctx context.Context, key string, data any) (*JSONDoc, error)                  // upsert, version++
	Update(ctx context.Context, key string, data any, expect int64) (*JSONDoc, error) // 乐观锁
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]*JSONDoc, error)
	// Watch 订阅前缀变更。返回取消函数。
	Watch(ctx context.Context, prefix string, fn func(JSONChange)) (unsubscribe func(), err error)
	Close() error
}

// ---- TSDB 风格 ----

// TSRecord 一条时间序列记录（measurement=Stream, dimensions=Tags, values=Fields）
type TSRecord struct {
	Ts     time.Time
	Stream string
	Tags   map[string]string
	Fields map[string]any
}

// TSQuery 时间范围 + 维度过滤
type TSQuery struct {
	Stream   string
	From, To time.Time
	Tag      map[string]string // 全部需匹配
	Limit    int
	Order    string // "asc" | "desc"
}

// TSAgg 聚合查询
type TSAgg struct {
	Stream   string
	From, To time.Time
	GroupBy  []string      // tag key 列表
	Bucket   time.Duration // 时间桶，0 = 不按桶
	Sum      []string      // 需要求和的 field key
	Where    map[string]string
}

// TSAggRow 聚合结果行
type TSAggRow struct {
	Tags   map[string]string
	Bucket time.Time
	Sums   map[string]float64
	Count  int64
}

// TSBackend 底层同步实现：批量写 + 查询 + 聚合
type TSBackend interface {
	AppendBatch(ctx context.Context, recs []*TSRecord) error
	Query(ctx context.Context, q TSQuery) ([]*TSRecord, error)
	Aggregate(ctx context.Context, q TSAgg) ([]TSAggRow, error)
	// DeleteByQuery 删除某 stream 在 before 之前的记录（rollup 裁剪旧 raw 用），返回删除条数
	DeleteByQuery(ctx context.Context, stream string, before time.Time) (int64, error)
	Close() error
}

// TSStore 追加式时间序列存储（Append 非阻塞入队，内部批量落盘）
type TSStore interface {
	Append(ctx context.Context, r *TSRecord) error
	Flush(ctx context.Context) error // 强制落盘（shutdown drain）
	Query(ctx context.Context, q TSQuery) ([]*TSRecord, error)
	Aggregate(ctx context.Context, q TSAgg) ([]TSAggRow, error)
	DeleteByQuery(ctx context.Context, stream string, before time.Time) (int64, error)
	Close() error
}

// Storage middleware 视角的存储聚合
type Storage struct {
	JSON JSONStore
	TS   TSStore
}

// 哨兵错误
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: version conflict")
)

// ---- 通用 watch 分发 ----

// WatchHub 把后端的变更事件分发到订阅者。
// 本地写（Put/Update/Delete）与外部变更（poller/LISTEN-NOTIFY）都走 Notify。
type WatchHub struct {
	ch   chan JSONChange
	mu   sync.Mutex
	subs []*sub
}

type sub struct {
	prefix string
	fn     func(JSONChange)
	active bool
}

func NewWatchHub() *WatchHub {
	h := &WatchHub{ch: make(chan JSONChange, 64)}
	go h.dispatch()
	return h
}

func (h *WatchHub) dispatch() {
	for c := range h.ch {
		h.mu.Lock()
		for _, s := range h.subs {
			if s.active && (s.prefix == "" || c.Prefix == "" || hasPrefix(c.Prefix, s.prefix)) {
				fn := s.fn
				go fn(c)
			}
		}
		h.mu.Unlock()
	}
}

func (h *WatchHub) Subscribe(prefix string, fn func(JSONChange)) (func(), error) {
	h.mu.Lock()
	s := &sub{prefix: prefix, fn: fn, active: true}
	h.subs = append(h.subs, s)
	h.mu.Unlock()
	return func() { h.mu.Lock(); s.active = false; h.mu.Unlock() }, nil
}

// Notify 非阻塞投递，满则丢弃（避免慢订阅者阻塞写路径）
func (h *WatchHub) Notify(c JSONChange) {
	select {
	case h.ch <- c:
	default:
	}
}

func (h *WatchHub) Close() {
	close(h.ch)
}

func hasPrefix(name, prefix string) bool {
	if len(name) < len(prefix) {
		return false
	}
	return name[:len(prefix)] == prefix
}
