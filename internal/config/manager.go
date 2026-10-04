package config

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lmgateway/internal/codingplan/codexauth"
	"lmgateway/internal/dispatch"
	"lmgateway/internal/pool"
	"lmgateway/internal/store"
)

// Manager 持有可热更的 Runtime（atomic.Pointer 原子换）。
// httpapi/admin 每请求取 Dispatcher()；配置变更经 store Watch 触发 Reload。
type Manager struct {
	rt atomic.Pointer[Runtime]

	js    store.JSONStore
	ts    store.TSStore
	codex *codexauth.Service
	base  Config
	pools *pool.Controller

	mu          sync.Mutex // 串行化 reload / recompile
	lastRuntime *Runtime   // last compiled runtime, for routing-only recompiles

	recompileMu sync.Mutex
	recompiling bool
	pending     bool
}

func NewManager(js store.JSONStore, ts store.TSStore, codex ...*codexauth.Service) *Manager {
	return NewManagerWithBase(js, ts, Config{}, codex...)
}

func NewManagerWithBase(js store.JSONStore, ts store.TSStore, base Config, codex ...*codexauth.Service) *Manager {
	var auth *codexauth.Service
	if len(codex) > 0 {
		auth = codex[0]
	}
	m := &Manager{js: js, ts: ts, codex: auth, base: base, pools: pool.NewController()}
	m.pools.SetNotifier(m.scheduleRecompile)
	return m
}

func (m *Manager) SetBase(base Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.base
	m.base = base
	if err := m.reloadLocked(context.Background()); err != nil {
		m.base = previous
		return err
	}
	return nil
}

func (m *Manager) Config(ctx context.Context) (Config, error) {
	return ConfigFromStoreWithBase(ctx, m.js, m.base)
}

func (m *Manager) Base() Config { return m.base }

// Build 用给定 Config 编译并发布（不做持久化；供 seed/测试直接使用）
func (m *Manager) Build(cfg Config) error {
	rt, err := Build(cfg, BuildDeps{Storage: m.storage(), Codex: m.codex, Pools: m.pools})
	if err != nil {
		return err
	}
	m.rt.Store(rt)
	return nil
}

// Reload 从 store 装载配置 → 编译（校验失败不换，旧配置继续跑）→ 原子换
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadLocked(ctx)
}

func (m *Manager) reloadLocked(ctx context.Context) error {
	cfg, err := ConfigFromStoreWithBase(ctx, m.js, m.base)
	if err != nil {
		log.Printf("[config] reload rejected: read store failed: %v", err)
		return err
	}
	rt, err := Build(cfg, BuildDeps{Storage: m.storage(), Codex: m.codex, Pools: m.pools})
	if err != nil {
		log.Printf("[config] reload rejected: %v", err)
		return err
	}
	m.rt.Store(rt)
	m.lastRuntime = rt
	discoveryEnabled := 0
	for _, p := range cfg.Providers {
		if p.Discover {
			discoveryEnabled++
			if p.Type != "openai" {
				log.Printf("[config] provider name=%s discover=true ignored: unsupported type=%s", p.Name, p.Type)
			}
		}
		log.Printf("[config] provider name=%s type=%s discover=%t", p.Name, p.Type, p.Discover)
	}
	log.Printf("[config] reloaded: providers=%d models=%d rules=%d discovery_enabled=%d", len(cfg.Providers), len(cfg.Models), len(cfg.Rules), discoveryEnabled)
	return nil
}

// recompileDebounce coalesces bursts of pool rotations into one routing rebuild.
const recompileDebounce = 300 * time.Millisecond

// Recompile rebuilds only the routing table from the last compiled config and
// the current pool state, reusing the existing registry so provider runtime
// state is preserved. Used by in-memory pool rotation (no config store writes).
func (m *Manager) Recompile() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastRuntime == nil {
		return nil
	}
	rt, err := rebuildRouting(m.lastRuntime.Config, m.lastRuntime, m.pools)
	if err != nil {
		log.Printf("[pools] recompile rejected: %v", err)
		return err
	}
	m.rt.Store(rt)
	m.lastRuntime = rt
	return nil
}

// scheduleRecompile debounces rotation-triggered recompiles: while one is in
// flight, further rotations set a pending flag and the loop runs once more.
func (m *Manager) scheduleRecompile() {
	m.recompileMu.Lock()
	if m.recompiling {
		m.pending = true
		m.recompileMu.Unlock()
		return
	}
	m.recompiling = true
	m.recompileMu.Unlock()
	go func() {
		for {
			time.Sleep(recompileDebounce)
			_ = m.Recompile()
			m.recompileMu.Lock()
			if m.pending {
				m.pending = false
				m.recompileMu.Unlock()
				continue
			}
			m.recompiling = false
			m.recompileMu.Unlock()
			return
		}
	}()
}

// Start 初始装载 + 订阅 store 变更自动热更。返回后应在 ctx 生命周期内运行。
func (m *Manager) Start(ctx context.Context) error {
	if err := m.Reload(ctx); err != nil {
		return err
	}
	unsub, err := m.js.Watch(ctx, "", func(store.JSONChange) {
		if err := m.Reload(ctx); err != nil {
			log.Printf("[config] reload failed (keep old config): %v", err)
		}
	})
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		unsub()
	}()
	return nil
}

func (m *Manager) Runtime() *Runtime { return m.rt.Load() }

func (m *Manager) Dispatcher() *dispatch.Dispatcher { return m.rt.Load().Dispatcher }

// Models 当前热更 config 的 model 实体
func (m *Manager) Models() []ModelCfg { return m.rt.Load().Config.Models }

// ListModels 聚合 /v1/models 库存：显式 models[]（无前缀）+ provider 热取发现（[provider]/ 前缀）。
// filter 只返回指定 provider；单个 provider 热取失败时，filter 场景返回错误，否则跳过失败。
func (m *Manager) ListModels(ctx context.Context, filter string) ([]ModelEntry, error) {
	rt := m.rt.Load()
	cfg := rt.Config

	// 显式 models[]（无前缀，精确匹配）
	explicitBare := map[string]map[string]bool{}
	entries := []ModelEntry{}
	seen := map[string]bool{}
	for _, mdl := range cfg.Models {
		if mdl.Name == "" || mdl.Provider == "" {
			continue
		}
		if filter != "" && mdl.Provider != filter {
			continue
		}
		bare := mdl.Name
		if strings.HasPrefix(mdl.Name, mdl.Provider+"/") {
			bare = strings.TrimPrefix(mdl.Name, mdl.Provider+"/")
		}
		if explicitBare[mdl.Provider] == nil {
			explicitBare[mdl.Provider] = map[string]bool{}
		}
		explicitBare[mdl.Provider][bare] = true
		if !seen[mdl.Name] {
			seen[mdl.Name] = true
			entries = append(entries, ModelEntry{ID: mdl.Name, OwnedBy: mdl.Provider})
		}
	}

	// provider 热取发现（[provider]/ 前缀）
	var failErr error
	for _, p := range cfg.Providers {
		if p.Name == "" || !p.Discover {
			continue
		}
		if filter != "" && p.Name != filter {
			continue
		}
		h, ok := rt.Registry.Get(p.Name)
		if !ok || h.Discover == nil {
			log.Printf("[models] provider %s discovery requested but provider has no discovery capability", p.Name)
			continue
		}
		models, err := h.Discover(ctx)
		if err != nil {
			if filter != "" {
				return nil, fmt.Errorf("provider %s: %w", p.Name, err)
			}
			log.Printf("[models] provider %s fetch failed (skipped): %v", p.Name, err)
			continue
		}
		for _, mo := range models {
			// 显式配置同一 provider 的同名模型已覆盖，不重复列前缀版本
			if explicitBare[p.Name][mo.ID] {
				continue
			}
			id := p.Name + "/" + mo.ID
			if seen[id] {
				continue
			}
			seen[id] = true
			entries = append(entries, ModelEntry{ID: id, OwnedBy: p.Name, Created: mo.Created, SpeedTiers: mo.SpeedTiers})
			for _, tier := range mo.SpeedTiers {
				if strings.EqualFold(tier, "fast") {
					fastID := p.Name + "/" + mo.ID + "-fast"
					if !seen[fastID] {
						seen[fastID] = true
						entries = append(entries, ModelEntry{ID: fastID, OwnedBy: p.Name, Created: mo.Created})
					}
				}
			}
		}
	}
	return entries, failErr
}

func (m *Manager) Storage() *store.Storage { return m.storage() }

// Pools is the backend-pool controller shared with the compiled runtime.
func (m *Manager) Pools() *pool.Controller { return m.pools }

func (m *Manager) storage() *store.Storage {
	return &store.Storage{JSON: m.js, TS: m.ts}
}

func (m *Manager) Close() {
	if m.js != nil {
		_ = m.js.Close()
	}
}
