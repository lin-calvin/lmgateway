package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lmgateway/internal/api"
	"lmgateway/internal/codingplan/codexauth"
	"lmgateway/internal/codingplan/codexui"
	"lmgateway/internal/config"
	"lmgateway/internal/httpapi"
	"lmgateway/internal/metrics"
	"lmgateway/internal/observability"
	"lmgateway/internal/rollup"
	"lmgateway/internal/store"
	"lmgateway/internal/store/pg"
	"lmgateway/internal/store/sqlite"
	"lmgateway/internal/tenancy"
	"lmgateway/internal/tenancyapi"
)

func main() {
	defaultConfig := os.Getenv("LMGATEWAY_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "config/lmgateway.yaml"
	}
	cfgPath := flag.String("config", defaultConfig, "seed config yaml path")
	defaultDB := os.Getenv("LMGATEWAY_DB_DSN")
	if defaultDB == "" {
		defaultDB = "sqlite://data/gateway.db"
	}
	dbDSN := flag.String("db", defaultDB, "storage dsn: sqlite://path | postgres://...")
	addrFlag := flag.String("addr", "", "listen addr (overrides config server.addr)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	js, ts, closeStore := openStore(ctx, *dbDSN)
	defer closeStore()

	// YAML is the read-only baseline; the database stores only overrides and
	// database-only entities.
	baseConfig, err := config.LoadConfig(*cfgPath)
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("load baseline config failed: %v", err)
	}
	if err := config.SeedFromFile(ctx, js, *cfgPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("reconcile baseline config failed: %v", err)
	}

	codex := codexauth.New(js, codexauth.Config{})
	m := config.NewManagerWithBase(js, ts, baseConfig, codex)

	// 多租户:租户/项目/用户/密钥仓储 + 配额限流器。
	// SetQuota 必须在 m.Start 之前调用,否则 authorize handler 不会被注册。
	tenancyRepo, err := tenancy.NewRepository(ctx, js)
	if err != nil {
		log.Fatalf("initialize tenancy store failed: %v", err)
	}
	quotaLimiter := tenancy.NewLimiter(tenancy.Options{})
	m.SetQuota(quotaLimiter)

	telemetryShutdown, err := observability.Init(ctx)
	if err != nil {
		log.Fatalf("initialize observability: %v", err)
	}
	defer func() { _ = telemetryShutdown(context.Background()) }()
	if err := m.Start(ctx); err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	// spend 热/冷分层：过期 raw → 日汇总（watermark 防重）
	rj := rollup.New(ts, js)
	rj.Start(ctx, 0)
	gatewayKey := os.Getenv("LMGATEWAY_API_KEY")
	keySource := "disabled"
	if gatewayKey != "" {
		keySource = "environment"
	}
	if serverCfg, err := config.LoadServerCfg(*cfgPath); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[auth] YAML master key unavailable: %v", err)
		}
	} else if serverCfg.MasterKey != "" {
		gatewayKey = serverCfg.MasterKey
		keySource = "yaml"
	}
	log.Printf("[auth] master key source=%s", keySource)
	tenancyAuth := tenancy.NewAuthenticator(gatewayKey, tenancyRepo)

	// 配额计数器是进程内的,重启会归零;从 spend 记录重建当日/当月成本,
	// 否则重启即可绕过日/月预算。之后每 5 分钟校准一次。
	if err := tenancy.Calibrate(ctx, ts, quotaLimiter); err != nil {
		log.Printf("[tenancy] quota calibration failed: %v", err)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := tenancy.Calibrate(ctx, ts, quotaLimiter); err != nil {
					log.Printf("[tenancy] quota calibration failed: %v", err)
				}
			}
		}
	}()

	addr := *addrFlag
	if addr == "" {
		addr = m.Runtime().Config.Server.Addr
	}
	if addr == "" {
		addr = ":8080"
	}

	mux := http.NewServeMux()
	mux.Handle("/ui/codex/", http.StripPrefix("/ui/codex", codexui.Handler()))
	mux.Handle("/metrics", metrics.New(ts))

	// 数据面:接受 master key 或租户 API key。
	mux.Handle("/", tenancyAuth.Guard(tenancy.ScopeData, httpapi.New(m)))
	// 多租户管理面(供前端 SPA 使用):接受 master key 或用户会话;
	// 登录与首次初始化不需要既有凭据(见 GuardSoft)。
	mux.Handle("/api/tenancy/", tenancyAuth.GuardSoft(tenancy.ScopeAdmin, tenancyapi.New(tenancyapi.Deps{
		Repo:       tenancyRepo,
		Auth:       tenancyAuth,
		Limiter:    quotaLimiter,
		TS:         ts,
		SessionTTL: 12 * time.Hour,
		Models: func() []string {
			out := []string{}
			for _, model := range m.Models() {
				if model.Name != "" {
					out = append(out, model.Name)
				}
			}
			return out
		},
		// 展示设置(汇率)从运行中的配置读,改完立即生效(配置管理器会热重载)
		Display: func() tenancyapi.DisplaySettings {
			fallback := tenancyapi.DisplaySettings{Currency: "USD", USDToCNY: config.DefaultDisplayCfg().USDToCNY}
			cfg, err := m.Config(context.Background())
			if err != nil {
				return fallback
			}
			display := config.NormalizeDisplayCfg(cfg.Display)
			return tenancyapi.DisplaySettings{Currency: display.Currency, USDToCNY: display.USDToCNY}
		},
	})))
	// 网关配置管理面(ops + 全局管理员):master key 或 role=admin 的会话。
	// 租户管理员/成员/数据面 API key 一律 403(见 tenancy.GuardConfig)。
	mux.Handle("/api/", tenancyAuth.GuardConfig(api.New(api.Deps{Manager: m, TS: ts, Rollup: rj, SeedFile: *cfgPath, Codex: codex, Pools: m.Pools()})))

	srv := &http.Server{Addr: addr, Handler: httpapi.WithCORS(mux)}
	go func() {
		log.Printf("gateway listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ts.Flush(shCtx)
	_ = ts.Close()
	_ = srv.Shutdown(shCtx)
}

// openStore 按 DSN 打开存储（SQLite 开发 / Postgres 生产），返回 JSONStore + TSStore + 关闭函数
func openStore(ctx context.Context, dsn string) (store.JSONStore, store.TSStore, func()) {
	switch {
	case strings.HasPrefix(dsn, "sqlite://"):
		d, err := sqlite.Open(ctx, strings.TrimPrefix(dsn, "sqlite://"))
		if err != nil {
			log.Fatalf("open sqlite: %v", err)
		}
		js, err := d.NewJSONStore(ctx)
		if err != nil {
			log.Fatalf("init sqlite store: %v", err)
		}
		ts := store.NewTS(d.NewTSBackend(), store.BufferedOpts{})
		return js, ts, func() {
			_ = js.Close()
			_ = ts.Close()
			_ = d.Close()
		}
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		d, err := pg.Open(ctx, dsn)
		if err != nil {
			log.Fatalf("open postgres: %v", err)
		}
		js, err := d.NewJSONStore(ctx)
		if err != nil {
			log.Fatalf("init postgres store: %v", err)
		}
		ts := store.NewTS(d.NewTSBackend(), store.BufferedOpts{})
		return js, ts, func() {
			_ = js.Close()
			_ = ts.Close()
			d.Close()
		}
	default:
		log.Fatalf("unknown db dsn %q (want sqlite:// or postgres://)", dsn)
		return nil, nil, nil
	}
}
