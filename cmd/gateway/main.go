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
	"lmgateway/internal/auth"
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
	gatewayAuth := auth.New(gatewayKey)

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
	mux.Handle("/", gatewayAuth.Wrap(httpapi.New(m)))
	mux.Handle("/api/", gatewayAuth.Wrap(api.New(api.Deps{Manager: m, TS: ts, Rollup: rj, SeedFile: *cfgPath, Codex: codex})))

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
