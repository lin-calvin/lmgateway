package store

import (
	"context"
	"log"
	"sync"
	"time"
)

// BufferedOpts 批量落盘参数
type BufferedOpts struct {
	Capacity   int           // 缓冲容量，0 = 1024
	BatchSize  int           // 触发批量写入的条数，0 = 500
	Interval   time.Duration // 周期性落盘间隔，0 = 1s
	DropOnFull bool          // 满时丢弃新记录（true）还是阻塞调用方（false）
}

// bufferedTS 给 TSBackend 加异步批量缓冲，实现 TSStore。
// Append 非阻塞入队（或满时阻塞/丢弃），后台 flusher 批量写。
type bufferedTS struct {
	backend  TSBackend
	opts     BufferedOpts
	ch       chan *TSRecord
	flushReq chan chan struct{} // Flush 同步请求
	stop     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
}

// NewTS 把同步后端包装成异步批量 TSStore
func NewTS(backend TSBackend, opts BufferedOpts) TSStore {
	if opts.Capacity <= 0 {
		opts.Capacity = 1024
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	b := &bufferedTS{
		backend:  backend,
		opts:     opts,
		ch:       make(chan *TSRecord, opts.Capacity),
		flushReq: make(chan chan struct{}),
		stop:     make(chan struct{}),
	}
	b.wg.Add(1)
	go b.loop()
	return b
}

func (b *bufferedTS) loop() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.opts.Interval)
	defer ticker.Stop()
	var batch []*TSRecord
	for {
		select {
		case r := <-b.ch:
			batch = append(batch, r)
			if len(batch) >= b.opts.BatchSize {
				b.flush(batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				b.flush(batch)
				batch = nil
			}
		case resp := <-b.flushReq:
			// 先把通道里已排队的全收进来，再落盘
		drain:
			for {
				select {
				case r := <-b.ch:
					batch = append(batch, r)
				default:
					break drain
				}
			}
			if len(batch) > 0 {
				b.flush(batch)
				batch = nil
			}
			close(resp)
		case <-b.stop:
			if len(batch) > 0 {
				b.flush(batch)
			}
			return
		}
	}
}

func (b *bufferedTS) flush(batch []*TSRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.backend.AppendBatch(ctx, batch); err != nil {
		log.Printf("[store] ts append batch failed (%d records): %v", len(batch), err)
	}
}

// Append 非阻塞入队；缓冲满时按 opts.DropOnFull 决定丢弃或阻塞
func (b *bufferedTS) Append(ctx context.Context, r *TSRecord) error {
	if b.opts.DropOnFull {
		select {
		case b.ch <- r:
		default:
			log.Printf("[store] ts buffer full, dropping record stream=%s", r.Stream)
		}
		return nil
	}
	select {
	case b.ch <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush 强制落盘剩余批次（shutdown drain）
func (b *bufferedTS) Flush(ctx context.Context) error {
	resp := make(chan struct{})
	select {
	case b.flushReq <- resp:
		select {
		case <-resp:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *bufferedTS) Query(ctx context.Context, q TSQuery) ([]*TSRecord, error) {
	return b.backend.Query(ctx, q)
}

func (b *bufferedTS) Aggregate(ctx context.Context, q TSAgg) ([]TSAggRow, error) {
	return b.backend.Aggregate(ctx, q)
}

// DeleteByQuery 同步下推后端（rollup 裁剪 raw 用；删除前先 Flush 保证未落盘记录已在库）
func (b *bufferedTS) DeleteByQuery(ctx context.Context, stream string, before time.Time) (int64, error) {
	return b.backend.DeleteByQuery(ctx, stream, before)
}

// Close 停止 flusher 并落盘剩余批次
func (b *bufferedTS) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.stop)
	b.wg.Wait()
	return b.backend.Close()
}
