package store

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MetaWatcher 用 meta version 轮询检测外部变更（跨进程）。
// 本地写通过 MarkLocal 同步已知版本，避免轮询对本进程自己的写入重复通知。
type MetaWatcher struct {
	getVersion func(ctx context.Context) (int64, error)
	notify     func(JSONChange)
	known      atomic.Int64
	stop       chan struct{}
	wg         sync.WaitGroup
}

func NewMetaWatcher(getVersion func(ctx context.Context) (int64, error), notify func(JSONChange), initial int64) *MetaWatcher {
	w := &MetaWatcher{getVersion: getVersion, notify: notify, stop: make(chan struct{})}
	w.known.Store(initial)
	return w
}

func (w *MetaWatcher) Start(ctx context.Context, interval time.Duration) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				v, err := w.getVersion(ctx)
				if err != nil {
					continue
				}
				if v != w.known.Load() {
					w.known.Store(v)
					w.notify(JSONChange{Prefix: ""})
				}
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (w *MetaWatcher) MarkLocal(v int64) { w.known.Store(v) }

func (w *MetaWatcher) Close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wg.Wait()
}
