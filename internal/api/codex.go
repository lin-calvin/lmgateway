package api

import (
	"encoding/json"
	"net/http"

	"lmgateway/internal/audit"
)

func (a *api) handleCodexDeviceStart(w http.ResponseWriter, r *http.Request) {
	c, err := a.deps.Codex.StartDevice(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *api) handleCodexDevicePoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.RequestID == "" {
		writeErr(w, http.StatusBadRequest, "request_id is required")
		return
	}
	result, err := a.deps.Codex.PollDevice(r.Context(), body.RequestID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *api) handleCodexStatus(w http.ResponseWriter, r *http.Request) {
	status, err := a.deps.Codex.Status(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *api) handleCodexLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.deps.Codex.Logout(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Record(r.Context(), "codex.logout", "auth/chatgpt_codex", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
