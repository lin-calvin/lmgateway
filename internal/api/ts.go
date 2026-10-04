package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lmgateway/internal/store"
)

func (a *api) handleTSQuery(w http.ResponseWriter, r *http.Request) {
	q := store.TSQuery{Stream: r.PathValue("stream"), Order: "desc"}
	var err error
	q.From, q.To, err = rangeFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			q.Limit = n
		}
	}
	tag := map[string]string{}
	for _, k := range []string{"provider", "model", "status", "stream"} {
		if v := r.URL.Query().Get(k); v != "" {
			tag[k] = v
		}
	}
	if len(tag) > 0 {
		q.Tag = tag
	}
	recs, err := a.deps.TS.Query(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs})
}

func (a *api) handleTSAgg(w http.ResponseWriter, r *http.Request) {
	q := store.TSAgg{
		Stream: r.PathValue("stream"),
		Sum:    []string{"prompt_tokens", "completion_tokens", "total_tokens", "reasoning_tokens", "cached_tokens", "cost", "latency_ms", "ttft_ms", "ttft_samples", "generation_ms", "generation_samples", "generation_tokens"},
	}
	var err error
	q.From, q.To, err = rangeFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if g := r.URL.Query().Get("group"); g != "" {
		for _, s := range strings.Split(g, ",") {
			if s = strings.TrimSpace(s); s != "" {
				q.GroupBy = append(q.GroupBy, s)
			}
		}
	}
	if b := r.URL.Query().Get("bucket"); b != "" {
		if d, err := time.ParseDuration(b); err == nil {
			q.Bucket = d
		}
	}
	if m := r.URL.Query().Get("model"); m != "" {
		q.Where = map[string]string{"model": m}
	}
	rows, err := a.deps.TS.Aggregate(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func rangeFromQuery(r *http.Request) (from, to time.Time, err error) {
	if s := r.URL.Query().Get("from"); s != "" {
		from, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from timestamp: %w", err)
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		to, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to timestamp: %w", err)
		}
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must not be after to")
	}
	return from, to, nil
}
