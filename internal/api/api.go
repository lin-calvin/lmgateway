// Package api 统一 API：以 handler 实体（provider/model）为中心，配置/TS/动作分层。
//
//	/api/{kind}/{verb}/{name}         kind=provider|model, verb=set|get|delete|patch|list
//	/api/config/rules/{verb}/{id}     set|get|delete|patch|list|meta
//	/api/config/settings/{verb}/{name} server|spend
//	/api/ts/{stream}/query|agg        TS 只读
//	/api/action/{name}                rollup/run, spend/flush, config/reload, config/seed
//
// 校验：JSON Schema（Go struct 生成，shape）+ 语义（候选 config.Build，不破不入盘）。
// 写入即触发 store Watch 热更。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/codingplan/codexauth"
	"lmgateway/internal/config"
	"lmgateway/internal/pool"
	"lmgateway/internal/rollup"
	"lmgateway/internal/store"
)

// Deps 依赖注入
type Deps struct {
	Manager  *config.Manager
	TS       store.TSStore
	Rollup   *rollup.Job
	SeedFile string
	Codex    *codexauth.Service
	Pools    *pool.Controller
}

// New 组装统一 API mux
func New(d Deps) http.Handler {
	a := &api{deps: d}
	a.init()
	mux := http.NewServeMux()

	// handler 实体（provider/model）：verb 紧跟 kind
	for _, e := range a.entities {
		kind := e.Kind
		mux.HandleFunc("GET /api/"+kind+"/list", a.handleList(e))
		mux.HandleFunc("GET /api/"+kind+"/get/{name}", a.handleGet(e))
		mux.HandleFunc("PUT /api/"+kind+"/set/{name}", a.handleSet(e))
		mux.HandleFunc("PATCH /api/"+kind+"/patch/{name}", a.handlePatch(e))
		mux.HandleFunc("DELETE /api/"+kind+"/delete/{name}", a.handleDelete(e))
		mux.HandleFunc("POST /api/"+kind+"/reset/{name}", a.handleReset(e))
	}

	// 网关配置（非实体）
	mux.HandleFunc("GET /api/config/rules/meta", a.handleRuleMeta)
	mux.HandleFunc("GET /api/config/rules/list", a.handleRuleList)
	mux.HandleFunc("GET /api/config/rules/get/{id}", a.handleRuleGet)
	mux.HandleFunc("PUT /api/config/rules/set/{id}", a.handleRuleSet)
	mux.HandleFunc("PATCH /api/config/rules/patch/{id}", a.handleRulePatch)
	mux.HandleFunc("DELETE /api/config/rules/delete/{id}", a.handleRuleDelete)
	mux.HandleFunc("POST /api/config/rules/reset/{id}", a.handleRuleReset)

	mux.HandleFunc("GET /api/config/settings/list", a.handleSettingList)
	mux.HandleFunc("GET /api/config/settings/get/{name}", a.handleSettingGet)
	mux.HandleFunc("PUT /api/config/settings/set/{name}", a.handleSettingSet)
	mux.HandleFunc("PATCH /api/config/settings/patch/{name}", a.handleSettingPatch)
	mux.HandleFunc("POST /api/config/settings/reset/{name}", a.handleSettingReset)

	// TS 只读
	mux.HandleFunc("GET /api/spend/token", a.handleToken)
	mux.HandleFunc("GET /api/ts/{stream}/query", a.handleTSQuery)
	mux.HandleFunc("GET /api/ts/{stream}/agg", a.handleTSAgg)

	// 动作
	mux.HandleFunc("POST /api/action/{name...}", a.handleAction)
	if d.Codex != nil {
		mux.HandleFunc("POST /api/action/codex/login/device/start", a.handleCodexDeviceStart)
		mux.HandleFunc("POST /api/action/codex/login/device/poll", a.handleCodexDevicePoll)
		mux.HandleFunc("GET /api/action/codex/status", a.handleCodexStatus)
		mux.HandleFunc("POST /api/action/codex/logout", a.handleCodexLogout)
	}

	return mux
}

type api struct {
	deps     Deps
	entities []*Entity
	schemas  map[string]*schemaBundle
	actions  map[string]func(context.Context, json.RawMessage) (any, error)
}

func (a *api) init() {
	a.entities = []*Entity{
		providerEntity(),
		modelEntity(),
		poolEntity(),
	}
	a.schemas = map[string]*schemaBundle{}
	for _, e := range a.entities {
		a.schemas[e.Kind] = mustSchema(e.DocType)
	}
	a.actions = map[string]func(context.Context, json.RawMessage) (any, error){
		"rollup/run": func(ctx context.Context, _ json.RawMessage) (any, error) {
			if a.deps.Rollup == nil {
				return nil, errors.New("rollup not available")
			}
			return map[string]any{"ok": true}, a.deps.Rollup.Run(ctx)
		},
		"spend/flush": func(ctx context.Context, _ json.RawMessage) (any, error) {
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return map[string]any{"ok": true}, a.deps.TS.Flush(c)
		},
		"config/reload": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return map[string]any{"ok": true}, a.deps.Manager.Reload(ctx)
		},
		"config/seed": func(ctx context.Context, _ json.RawMessage) (any, error) {
			if a.deps.SeedFile == "" {
				return nil, errors.New("no seed file configured")
			}
			base, err := config.LoadConfig(a.deps.SeedFile)
			if err != nil {
				return nil, err
			}
			if err := config.ReconcileBaseline(ctx, a.deps.Manager.Storage().JSON, base); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, a.deps.Manager.SetBase(base)
		},
	}
}

// ---- 通用写入流程 ----

func (a *api) applyAndWrite(w http.ResponseWriter, r *http.Request, e *Entity, name string, body []byte) {
	ctx := r.Context()
	doc := e.New()
	if err := json.Unmarshal(body, doc); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := a.schemas[e.Kind].Validate(doc); err != nil {
		log.Printf("[config] write rejected kind=%s name=%s: schema: %v", e.Kind, name, err)
		writeErr(w, http.StatusUnprocessableEntity, "schema: "+err.Error())
		return
	}
	if err := e.checkRequired(doc); err != nil {
		log.Printf("[config] write rejected kind=%s name=%s: %v", e.Kind, name, err)
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	cfg, err := a.deps.Manager.Config(ctx)
	if err != nil {
		log.Printf("[config] write rejected kind=%s name=%s: read config failed: %v", e.Kind, name, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := e.Apply(&cfg, name, doc); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, err := config.Build(cfg, config.BuildDeps{Storage: a.deps.Manager.Storage(), Codex: a.deps.Codex, Pools: a.deps.Pools}); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "config invalid: "+err.Error())
		return
	}
	base := a.deps.Manager.Base()
	source := config.SourceDB
	if _, ok := config.BaselineItem(base, e.Prefix+name); ok {
		source = config.SourceDBOverride
	}
	item, err := config.EncodeItem(source, doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	key := e.Prefix + name
	var written *store.JSONDoc
	if v := r.Header.Get("If-Match"); v != "" {
		expect, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad If-Match version")
			return
		}
		written, err = a.deps.Manager.Storage().JSON.Update(ctx, key, item, expect)
		if err == store.ErrConflict {
			writeErr(w, http.StatusConflict, "version conflict, refetch and retry")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		written, err = a.deps.Manager.Storage().JSON.Put(ctx, key, item)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "source": source, "version": written.Version})
}

func (a *api) loadCandidate(ctx context.Context) (config.Config, error) {
	return a.deps.Manager.Config(ctx)
}

// ---- handlers ----

func (a *api) handleList(e *Entity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prefix := e.Prefix
		base := a.deps.Manager.Base()
		items, err := config.ListResolvedItems(r.Context(), a.deps.Manager.Storage().JSON, base, prefix)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			m := map[string]any{}
			if err := json.Unmarshal(item.Data, &m); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if e.Mask != nil {
				m = e.Mask(m)
			}
			m["name"] = strings.TrimPrefix(item.Key, prefix)
			m["source"] = item.Source
			if item.Version > 0 {
				m["version"] = item.Version
			}
			out = append(out, m)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})
	}
}

func (a *api) handleGet(e *Entity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		base := a.deps.Manager.Base()
		item, err := config.ResolveItem(r.Context(), a.deps.Manager.Storage().JSON, base, e.Prefix+name)
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "not found: "+name)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		m := map[string]any{}
		if err := json.Unmarshal(item.Data, &m); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if e.Mask != nil {
			m = e.Mask(m)
		}
		m["name"] = name
		m["source"] = item.Source
		if item.Version > 0 {
			m["version"] = item.Version
		}
		writeJSON(w, http.StatusOK, m)
	}
}

func (a *api) handleSet(e *Entity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := readBody(w, r)
		if body == nil {
			return
		}
		a.applyAndWrite(w, r, e, r.PathValue("name"), body)
	}
}

func (a *api) handlePatch(e *Entity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := r.PathValue("name")
		key := e.Prefix + name
		base := a.deps.Manager.Base()
		resolved, err := config.ResolveItem(ctx, a.deps.Manager.Storage().JSON, base, key)
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "not found: "+name)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		merged, err := mergePatch(resolved.Data, readBody(w, r))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "patch: "+err.Error())
			return
		}
		a.applyAndWrite(w, r, e, name, merged)
	}
}

func entityData(e *Entity, cfg config.Config, name string) ([]byte, error) {
	var value any
	switch e.Kind {
	case "provider":
		for _, item := range cfg.Providers {
			if item.Name == name {
				value = item
				break
			}
		}
	case "model":
		for _, item := range cfg.Models {
			if item.Name == name {
				value = item
				break
			}
		}
	default:
		return nil, fmt.Errorf("unsupported entity kind %q", e.Kind)
	}
	if value == nil {
		return nil, store.ErrNotFound
	}
	return json.Marshal(value)
}

func (a *api) handleReset(e *Entity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		key := e.Prefix + name
		base := a.deps.Manager.Base()
		if _, ok := config.BaselineItem(base, key); !ok {
			writeErr(w, http.StatusUnprocessableEntity, "resource has no YAML baseline; use delete instead")
			return
		}
		doc, err := a.deps.Manager.Storage().JSON.Get(r.Context(), key)
		if err == store.ErrNotFound {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "source": config.SourceYAML})
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		item, err := a.deps.Manager.Storage().JSON.Put(r.Context(), key, map[string]any{"source": config.SourceYAML})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "source": config.SourceYAML, "version": item.Version, "previous_version": doc.Version})
	}
}

func (a *api) handleDelete(e *Entity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := r.PathValue("name")
		cfg, err := a.loadCandidate(ctx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := e.Remove(&cfg, name); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if _, err := config.Build(cfg, config.BuildDeps{Storage: a.deps.Manager.Storage(), Codex: a.deps.Codex, Pools: a.deps.Pools}); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "cannot delete: "+err.Error())
			return
		}
		if err := a.deps.Manager.Storage().JSON.Delete(ctx, e.Prefix+name); err != nil {
			if err == store.ErrNotFound {
				writeErr(w, http.StatusNotFound, "not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (a *api) handleAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	fn, ok := a.actions[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown action: "+name)
		return
	}
	out, err := fn(r.Context(), readBody(w, r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- helpers ----

func readBody(w http.ResponseWriter, r *http.Request) []byte {
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return nil
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
