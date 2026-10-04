package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"lmgateway/internal/config"
	"lmgateway/internal/store"
)

var allowedSettings = map[string]bool{"server": true, "spend": true, "logprobs": true, "display": true}

func (a *api) handleSettingList(w http.ResponseWriter, r *http.Request) {
	base, err := a.deps.Manager.Config(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := config.ListResolvedItems(r.Context(), a.deps.Manager.Storage().JSON, base, "setting/")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var value map[string]any
		if err := json.Unmarshal(item.Data, &value); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{
			"name":    strings.TrimPrefix(item.Key, "setting/"),
			"value":   value,
			"source":  item.Source,
			"version": item.Version,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (a *api) handleSettingGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !allowedSettings[name] {
		writeErr(w, http.StatusNotFound, "unknown setting: "+name)
		return
	}
	base, err := a.deps.Manager.Config(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err := config.ResolveItem(r.Context(), a.deps.Manager.Storage().JSON, base, config.SettingKey(name))
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "setting not found")
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
	value["version"] = item.Version
	writeJSON(w, http.StatusOK, value)
}

func (a *api) handleSettingSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !allowedSettings[name] {
		writeErr(w, http.StatusNotFound, "unknown setting: "+name)
		return
	}
	body := readBody(w, r)
	if body == nil {
		return
	}
	if err := validateSetting(name, body); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	source := config.SourceDB
	if _, ok := config.BaselineItem(a.deps.Manager.Base(), config.SettingKey(name)); ok {
		source = config.SourceDBOverride
	}
	item, err := config.EncodeItem(source, json.RawMessage(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := a.deps.Manager.Storage().JSON.Put(r.Context(), config.SettingKey(name), item); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (a *api) handleSettingPatch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !allowedSettings[name] {
		writeErr(w, http.StatusNotFound, "unknown setting: "+name)
		return
	}
	base, err := a.deps.Manager.Config(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	current, err := config.ResolveItem(r.Context(), a.deps.Manager.Storage().JSON, base, config.SettingKey(name))
	if err == store.ErrNotFound {
		writeErr(w, http.StatusNotFound, "setting not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	merged, err := mergePatch(current.Data, readBody(w, r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "patch: "+err.Error())
		return
	}
	if err := validateSetting(name, merged); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	item, err := config.EncodeItem(config.SourceDBOverride, json.RawMessage(merged))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	written, err := a.deps.Manager.Storage().JSON.Put(r.Context(), config.SettingKey(name), item)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "source": config.SourceDBOverride, "version": written.Version})
}

func (a *api) handleSettingReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !allowedSettings[name] {
		writeErr(w, http.StatusNotFound, "unknown setting: "+name)
		return
	}
	key := config.SettingKey(name)
	base := a.deps.Manager.Base()
	if _, ok := config.BaselineItem(base, key); !ok {
		writeErr(w, http.StatusUnprocessableEntity, "setting has no YAML baseline")
		return
	}
	item, err := a.deps.Manager.Storage().JSON.Put(r.Context(), key, map[string]any{"source": config.SourceYAML})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "source": config.SourceYAML, "version": item.Version})
}

func validateSetting(name string, body []byte) error {
	switch name {
	case "server":
		var s config.ServerCfg
		return json.Unmarshal(body, &s)
	case "spend":
		var s config.SpendCfg
		if err := json.Unmarshal(body, &s); err != nil {
			return err
		}
		if s.RawRetentionDays <= 0 {
			s.RawRetentionDays = config.DefaultSpendCfg().RawRetentionDays
		}
		if len(s.RollupDimensions) == 0 {
			s.RollupDimensions = config.DefaultSpendCfg().RollupDimensions
		}
		return nil
	case "logprobs":
		var s config.LogprobsCfg
		return json.Unmarshal(body, &s)
	case "display":
		var s config.DisplayCfg
		if err := json.Unmarshal(body, &s); err != nil {
			return err
		}
		// 币种/汇率非法时明确报错，不静默回落（否则"我改了汇率却没生效"极难排查）
		return config.ValidateDisplayCfg(s)
	}
	return nil
}
