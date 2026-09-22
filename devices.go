package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type deviceHub struct {
	mu          sync.Mutex
	subscribers map[int64]map[chan []byte]struct{}
}

type device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Active   bool   `json:"active"`
	Online   bool   `json:"online"`
	LastSeen string `json:"lastSeen"`
}

type deviceEvent struct {
	Type     string  `json:"type"`
	Action   string  `json:"action,omitempty"`
	DeviceID string  `json:"deviceId,omitempty"`
	Position float64 `json:"position,omitempty"`
}

func (application *app) registerDevice(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	if !deviceIDValid(payload.ID) || payload.Name == "" || len(payload.Name) > 80 {
		writeError(response, http.StatusBadRequest, "Некорректное устройство")
		return
	}
	tx, err := application.db.Begin(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось зарегистрировать устройство")
		return
	}
	defer tx.Rollback(request.Context())
	var hasActive bool
	if err = tx.QueryRow(request.Context(), "SELECT EXISTS(SELECT 1 FROM devices WHERE user_id = $1 AND is_active AND last_seen > NOW() - INTERVAL '45 seconds')", currentUser.ID).Scan(&hasActive); err == nil && !hasActive {
		_, err = tx.Exec(request.Context(), "UPDATE devices SET is_active = FALSE WHERE user_id = $1", currentUser.ID)
	}
	if err == nil {
		_, err = tx.Exec(request.Context(), `
			INSERT INTO devices (id, user_id, name, is_active) VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, id) DO UPDATE SET name = EXCLUDED.name, last_seen = NOW(), is_active = devices.is_active OR EXCLUDED.is_active`, payload.ID, currentUser.ID, payload.Name, !hasActive)
	}
	if err != nil || tx.Commit(request.Context()) != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось зарегистрировать устройство")
		return
	}
	if !hasActive {
		application.devices.broadcast(currentUser.ID, deviceEvent{Type: "device", DeviceID: payload.ID})
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) listDevices(response http.ResponseWriter, request *http.Request, currentUser user) {
	rows, err := application.db.Query(request.Context(), "SELECT id, name, is_active, TRUE, last_seen FROM devices WHERE user_id = $1 AND last_seen > NOW() - INTERVAL '45 seconds' ORDER BY is_active DESC, name", currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить устройства")
		return
	}
	defer rows.Close()
	items := []device{}
	for rows.Next() {
		var item device
		var lastSeen time.Time
		if err := rows.Scan(&item.ID, &item.Name, &item.Active, &item.Online, &lastSeen); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать устройства")
			return
		}
		item.LastSeen = lastSeen.Format(time.RFC3339)
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) activateDevice(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		ID string `json:"id"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if !deviceIDValid(payload.ID) {
		writeError(response, http.StatusBadRequest, "Некорректное устройство")
		return
	}
	tx, err := application.db.Begin(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось выбрать устройство")
		return
	}
	_, err = tx.Exec(request.Context(), "UPDATE devices SET is_active = FALSE WHERE user_id = $1", currentUser.ID)
	var affected int64
	if err == nil {
		result, updateErr := tx.Exec(request.Context(), "UPDATE devices SET is_active = TRUE WHERE user_id = $1 AND id = $2 AND last_seen > NOW() - INTERVAL '45 seconds'", currentUser.ID, payload.ID)
		err = updateErr
		affected = result.RowsAffected()
	}
	if err != nil || affected == 0 || tx.Commit(request.Context()) != nil {
		_ = tx.Rollback(request.Context())
		writeError(response, http.StatusNotFound, "Устройство не найдено")
		return
	}
	application.devices.broadcast(currentUser.ID, deviceEvent{Type: "device", DeviceID: payload.ID})
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) deleteDevice(response http.ResponseWriter, request *http.Request, currentUser user) {
	id := request.PathValue("id")
	if !deviceIDValid(id) {
		writeError(response, http.StatusBadRequest, "Некорректное устройство")
		return
	}
	tx, err := application.db.Begin(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось удалить устройство")
		return
	}
	defer tx.Rollback(request.Context())
	var wasActive bool
	if err = tx.QueryRow(request.Context(), "DELETE FROM devices WHERE user_id = $1 AND id = $2 RETURNING is_active", currentUser.ID, id).Scan(&wasActive); err != nil {
		writeError(response, http.StatusNotFound, "Устройство не найдено")
		return
	}
	activeID := ""
	if wasActive {
		_ = tx.QueryRow(request.Context(), `UPDATE devices SET is_active = TRUE WHERE user_id = $1 AND id = (
			SELECT id FROM devices WHERE user_id = $1 AND last_seen > NOW() - INTERVAL '45 seconds' ORDER BY last_seen DESC LIMIT 1
		) RETURNING id`, currentUser.ID).Scan(&activeID)
	}
	if err = tx.Commit(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось удалить устройство")
		return
	}
	application.devices.broadcast(currentUser.ID, deviceEvent{Type: "device", DeviceID: activeID})
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) playbackControl(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload deviceEvent
	if !decodeJSON(response, request, &payload) {
		return
	}
	allowed := map[string]bool{"load": true, "toggle": true, "play": true, "pause": true, "next": true, "previous": true, "seek": true}
	if !allowed[payload.Action] || payload.Position < 0 {
		writeError(response, http.StatusBadRequest, "Некорректная команда")
		return
	}
	payload.Type = "control"
	application.devices.broadcast(currentUser.ID, payload)
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) deviceEvents(response http.ResponseWriter, request *http.Request, currentUser user) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, http.StatusInternalServerError, "Поток событий недоступен")
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")
	channel := application.devices.subscribe(currentUser.ID)
	defer application.devices.unsubscribe(currentUser.ID, channel)
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case data := <-channel:
			_, _ = fmt.Fprintf(response, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(response, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (hub *deviceHub) subscribe(userID int64) chan []byte {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.subscribers == nil {
		hub.subscribers = map[int64]map[chan []byte]struct{}{}
	}
	if hub.subscribers[userID] == nil {
		hub.subscribers[userID] = map[chan []byte]struct{}{}
	}
	channel := make(chan []byte, 16)
	hub.subscribers[userID][channel] = struct{}{}
	return channel
}

func (hub *deviceHub) unsubscribe(userID int64, channel chan []byte) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	delete(hub.subscribers[userID], channel)
	close(channel)
}

func (hub *deviceHub) broadcast(userID int64, event deviceEvent) {
	data, _ := json.Marshal(event)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for channel := range hub.subscribers[userID] {
		select {
		case channel <- data:
		default:
		}
	}
}

func deviceIDValid(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}
