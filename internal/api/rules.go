package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
)

// matchFields 可匹配字段词汇表（只内容字段，不含位置）；xxx.* 为通配
var matchFields = []string{
	"type", "model", "stream", "metadata.*", "raw.*",
	"provider", "error", "error_kind",
}

// validMatchField 字段必须在词汇表或匹配通配（写时校验，避免 typo 静默不命中）
func validMatchField(f string) bool {
	for _, v := range matchFields {
		if f == v {
			return true
		}
		if strings.HasSuffix(v, ".*") && strings.HasPrefix(f, strings.TrimSuffix(v, "*")) {
			return true
		}
	}
	return false
}

func (a *api) handleRuleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"fields":    matchFields,
		"ops":       []string{"eq", "neq", "in", "prefix", "regex", "exists", "empty"},
		"entry":     "http",
		"terminals": []string{"respond", "error"},
		"semantics": map[string]any{
			"set":     "override: always writes, client value is replaced",
			"default": "apply-if-absent: writes only when the field is missing, nil or empty",
		},
	})
}

// 有效规则全集（编译后，含自动边 + 全局 respond）
func (a *api) handleRuleList(w http.ResponseWriter, r *http.Request) {
	rules := a.deps.Manager.Runtime().Table.All()
	out := make([]map[string]any, 0, len(rules))
	for _, rl := range rules {
		var conds []map[string]any
		for _, c := range rl.Match.Conds {
			conds = append(conds, map[string]any{"field": c.Field, "op": c.Op, "value": c.Value})
		}
		out = append(out, map[string]any{
			"id":        rl.ID,
			"from":      rl.From,
			"action":    rl.Action,
			"to":        rl.To,
			"set":       rl.Set,
			"default":   rl.Default,
			"match":     conds,
			"generated": rl.Generated,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (a *api) handleRuleGet(w http.ResponseWriter, r *http.Request) {
	base, err := a.deps.Manager.Config(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err := config.ResolveItem(r.Context(), a.deps.Manager.Storage().JSON, base, config.RuleKey(r.PathValue("id")))
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var value map[string]any
	if err := json.Unmarshal(item.Data, &value); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	value["source"] = item.Source
	if item.Version > 0 {
		value["version"] = item.Version
	}
	writeJSON(w, http.StatusOK, value)
}

func (a *api) ruleApply(w http.ResponseWriter, r *http.Request, body []byte, replace bool) {
	id := r.PathValue("id")
	var rl config.RuleCfg
	if err := json.Unmarshal(body, &rl); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid rule json: "+err.Error())
		return
	}
	rl.ID = id
	if rl.From == "" {
		writeErr(w, http.StatusUnprocessableEntity, "from is required")
		return
	}
	if rl.Action == "" && rl.To == "" {
		writeErr(w, http.StatusUnprocessableEntity, "action or to is required")
		return
	}
	if rl.Action == "" && len(rl.Set) == 0 && len(rl.Default) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "pure transform rule needs set or default")
		return
	}
	for _, c := range rl.Match {
		if !validMatchField(c.Field) {
			log.Printf("[config] write rejected kind=rule name=%s: unknown match field=%s", id, c.Field)
			writeErr(w, http.StatusUnprocessableEntity, "unknown match field: "+c.Field)
			return
		}
	}
	cfg, err := a.deps.Manager.Config(r.Context())
	if err != nil {
		log.Printf("[config] write rejected kind=rule name=%s: read store failed: %v", id, err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	replaced := false
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == id {
			cfg.Rules[i] = rl
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Rules = append(cfg.Rules, rl)
	}
	if _, err := config.Build(cfg, config.BuildDeps{Storage: a.deps.Manager.Storage(), Codex: a.deps.Codex, Pools: a.deps.Pools}); err != nil {
		log.Printf("[config] write rejected kind=rule name=%s: config invalid: %v", id, err)
		writeErr(w, http.StatusUnprocessableEntity, "config invalid: "+err.Error())
		return
	}
	source := config.SourceDB
	if _, ok := config.BaselineItem(a.deps.Manager.Base(), config.RuleKey(id)); ok {
		source = config.SourceDBOverride
	}
	item, err := config.EncodeItem(source, rl)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	key := config.RuleKey(id)
	var written *store.JSONDoc
	if v := r.Header.Get("If-Match"); v != "" {
		expect, parseErr := strconv.ParseInt(v, 10, 64)
		if parseErr != nil {
			writeErr(w, http.StatusBadRequest, "bad If-Match version")
			return
		}
		written, err = a.deps.Manager.Storage().JSON.Update(r.Context(), key, item, expect)
		if err == store.ErrConflict {
			writeErr(w, http.StatusConflict, "version conflict, refetch and retry")
			return
		}
	} else {
		written, err = a.deps.Manager.Storage().JSON.Put(r.Context(), key, item)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "source": source, "version": written.Version})
}

func (a *api) handleRuleSet(w http.ResponseWriter, r *http.Request) {
	body := readBody(w, r)
	if body == nil {
		return
	}
	a.ruleApply(w, r, body, false)
}

func (a *api) handleRulePatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	base := a.deps.Manager.Base()
	currentItem, err := config.ResolveItem(r.Context(), a.deps.Manager.Storage().JSON, base, config.RuleKey(id))
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	merged, err := mergePatch(currentItem.Data, readBody(w, r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "patch: "+err.Error())
		return
	}
	a.ruleApply(w, r, merged, true)
}

func (a *api) handleRuleReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := config.RuleKey(id)
	base := a.deps.Manager.Base()
	if _, ok := config.BaselineItem(base, key); !ok {
		writeErr(w, http.StatusUnprocessableEntity, "rule has no YAML baseline; use delete instead")
		return
	}
	item, err := a.deps.Manager.Storage().JSON.Put(r.Context(), key, map[string]any{"source": config.SourceYAML})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "source": config.SourceYAML, "version": item.Version})
}

func (a *api) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := a.deps.Manager.Config(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []config.RuleCfg
	for _, rl := range cfg.Rules {
		if rl.ID != id {
			out = append(out, rl)
		}
	}
	if len(out) == len(cfg.Rules) {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	cfg.Rules = out
	if _, err := config.Build(cfg, config.BuildDeps{Storage: a.deps.Manager.Storage(), Codex: a.deps.Codex, Pools: a.deps.Pools}); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "cannot delete: "+err.Error())
		return
	}
	if err := a.deps.Manager.Storage().JSON.Delete(r.Context(), config.RuleKey(id)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
