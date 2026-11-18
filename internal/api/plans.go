package api

import (
	"net/http"

	"lmgateway/internal/config"
)

func (a *api) handlePlansStatus(w http.ResponseWriter, r *http.Request) {
	if a.deps.Plans == nil {
		writeJSON(w, http.StatusOK, map[string]any{"plans": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": a.deps.Plans.Status()})
}

func (a *api) handlePlanPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"presets": config.PlanPresets()})
}
