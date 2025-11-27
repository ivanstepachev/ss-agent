package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
)

type httpHandler func(http.ResponseWriter, *http.Request)

func (a *Agent) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/adduser", a.wrap(a.handleAddUser))
	mux.Handle("/deleteuser", a.wrap(a.handleDeleteUser))
	mux.Handle("/reload", a.wrap(a.handleReload))
	mux.Handle("/restart", a.wrap(a.handleRestart))
	mux.HandleFunc("/stats", a.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func (a *Agent) wrap(handler httpHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if a.cfg.AuthToken != "" {
			if got := r.Header.Get("X-Auth-Token"); got != a.cfg.AuthToken {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		handler(w, r)
	})
}

func (a *Agent) handleAddUser(w http.ResponseWriter, r *http.Request) {
	a.opLock.RLock()
	defer a.opLock.RUnlock()

	var req struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	slot, err := a.store.AllocateSlot(r.Context(), req.UserID)
	if err != nil {
		switch {
		case errors.Is(err, errNoFreePorts):
			writeError(w, http.StatusConflict, "no_free_ports")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	shard, ok := a.shardMap[slot.ShardID]
	if !ok {
		writeError(w, http.StatusInternalServerError, "unknown_shard")
		return
	}
	statsByShard, totals, err := a.store.SlotStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stats_error")
		return
	}

	freeByShard := make(map[int]int, len(statsByShard))
	for _, sh := range a.shards {
		freeByShard[sh.ID] = statsByShard[sh.ID].Free
	}

	resp := map[string]any{
		"status":     "ok",
		"slotId":     slot.ID,
		"shardId":    shard.ID,
		"listenPort": shard.Port,
		"password":   fmt.Sprintf("%s:%s", a.store.ServerPassword(shard.ID), slot.Password),
		"method":     a.cfg.Method,
		"ip":         a.cfg.PublicIP,
		"freeSlots":  totals.Free,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	a.opLock.RLock()
	defer a.opLock.RUnlock()

	var req struct {
		SlotID  int   `json:"slotId"`
		SlotIDs []int `json:"slotIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	var targets []int
	if len(req.SlotIDs) > 0 {
		targets = req.SlotIDs
	} else if req.SlotID != 0 {
		targets = []int{req.SlotID}
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "slot_required")
		return
	}
	for _, id := range targets {
		err := a.store.ReserveSlot(r.Context(), id)
		if err != nil {
			switch {
			case errors.Is(err, errSlotNotFound):
				writeError(w, http.StatusNotFound, "slot_not_found")
			case errors.Is(err, errSlotReserved):
				writeError(w, http.StatusBadRequest, "already_reserved")
			case errors.Is(err, errSlotFree):
				writeError(w, http.StatusBadRequest, "slot_not_in_use")
			case errors.Is(err, errSlotNotInUse):
				writeError(w, http.StatusBadRequest, "slot_not_in_use")
			default:
				writeError(w, http.StatusInternalServerError, "internal_error")
			}
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *Agent) handleReload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShardID int `json:"shardId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "accepted",
		"message": "reload started",
	})

	var target []int
	if req.ShardID > 0 {
		target = []int{req.ShardID}
	}

	go func() {
		processed, err := a.Reload(context.Background(), true, target)
		if err != nil {
			log.Printf("async reload failed: %v", err)
			return
		}
		log.Printf("async reload finished: %+v", processed)
	}()
}

func (a *Agent) handleRestart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShardID int `json:"shardId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "accepted",
		"message": "restart started",
	})

	var target []int
	if req.ShardID > 0 {
		target = []int{req.ShardID}
	}

	go func() {
		processed, err := a.ReloadAndRestart(context.Background(), true, target)
		if err != nil {
			log.Printf("async restart failed: %v", err)
			return
		}
		log.Printf("async restart finished: %+v", processed)
	}()
}

func (a *Agent) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if a.cfg.AuthToken != "" {
		if got := r.Header.Get("X-Auth-Token"); got != a.cfg.AuthToken {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	a.opLock.RLock()
	defer a.opLock.RUnlock()

	statsByShard, totals, err := a.store.SlotStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stats_error")
		return
	}

	resp := struct {
		Shards []struct {
			ID       int `json:"id"`
			Port     int `json:"port"`
			Free     int `json:"free"`
			Used     int `json:"used"`
			Reserved int `json:"reserved"`
		} `json:"shards"`
		Totals SlotCounts `json:"totals"`
	}{
		Totals: totals,
	}

	for _, shard := range a.shards {
		counts := statsByShard[shard.ID]
		resp.Shards = append(resp.Shards, struct {
			ID       int `json:"id"`
			Port     int `json:"port"`
			Free     int `json:"free"`
			Used     int `json:"used"`
			Reserved int `json:"reserved"`
		}{
			ID:       shard.ID,
			Port:     shard.Port,
			Free:     counts.Free,
			Used:     counts.Used,
			Reserved: counts.Reserved,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{
		"status": "error",
		"error":  code,
	})
}
