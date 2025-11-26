package main

import (
	"context"
	"encoding/json"
	"errors"
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
	resp := map[string]any{
		"status":         "ok",
		"port":           slot.Port,
		"listenPort":     a.cfg.MinPort,
		"password":       slot.Password,
		"serverPassword": a.store.ServerPassword(),
		"method":         a.cfg.Method,
		"ip":             a.cfg.PublicIP,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if req.Port == 0 {
		writeError(w, http.StatusBadRequest, "port_required")
		return
	}
	err := a.store.ReserveSlot(r.Context(), req.Port)
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *Agent) handleReload(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "accepted",
		"message": "reload started",
	})

	go func() {
		processed, err := a.Reload(context.Background(), true)
		if err != nil {
			log.Printf("async reload failed: %v", err)
			return
		}
		log.Printf("async reload finished, reserved processed=%d", processed)
	}()
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
