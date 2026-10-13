package api

import (
	"encoding/json"
	"net/http"
)

// handlePoolStatus returns the runtime status (active backend, cooldowns) of a
// pool, which is not part of the persisted config document.
func (a *api) handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	if a.deps.Pools == nil {
		writeErr(w, http.StatusNotImplemented, "pools are not enabled")
		return
	}
	name := r.PathValue("name")
	status, ok := a.deps.Pools.Status(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "pool not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handlePoolRotate forces the pool to the next healthy backend.
func (a *api) handlePoolRotate(w http.ResponseWriter, r *http.Request) {
	if a.deps.Pools == nil {
		writeErr(w, http.StatusNotImplemented, "pools are not enabled")
		return
	}
	name := r.PathValue("name")
	active, ok := a.deps.Pools.Rotate(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "pool not found: "+name)
		return
	}
	status, _ := a.deps.Pools.Status(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active": active, "status": status})
}

// handlePoolClear drops all cooldowns without changing the active backend.
func (a *api) handlePoolClear(w http.ResponseWriter, r *http.Request) {
	if a.deps.Pools == nil {
		writeErr(w, http.StatusNotImplemented, "pools are not enabled")
		return
	}
	name := r.PathValue("name")
	if !a.deps.Pools.ClearCooldowns(name) {
		writeErr(w, http.StatusNotFound, "pool not found: "+name)
		return
	}
	status, _ := a.deps.Pools.Status(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

// handlePoolRecover resets a single backend of a pool to healthy (clears its
// fault), e.g. after topping up an out-of-balance account.
func (a *api) handlePoolRecover(w http.ResponseWriter, r *http.Request) {
	if a.deps.Pools == nil {
		writeErr(w, http.StatusNotImplemented, "pools are not enabled")
		return
	}
	name := r.PathValue("name")
	var body struct {
		Backend string `json:"backend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.Backend == "" {
		writeErr(w, http.StatusBadRequest, "backend is required")
		return
	}
	if !a.deps.Pools.Recover(name, body.Backend) {
		writeErr(w, http.StatusNotFound, "pool or backend not found")
		return
	}
	status, _ := a.deps.Pools.Status(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}
