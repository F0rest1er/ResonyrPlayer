package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type playlist struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Owner       string  `json:"owner"`
	Permission  string  `json:"permission"`
	TrackCount  int     `json:"trackCount"`
	TrackIDs    []int64 `json:"trackIds"`
	HasCover    bool    `json:"hasCover"`
}

func (application *app) tracksByIDs(response http.ResponseWriter, request *http.Request, currentUser user) {
	values := strings.Split(request.URL.Query().Get("ids"), ",")
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		if id, err := strconv.ParseInt(value, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > 1000 {
		writeJSON(response, http.StatusOK, []track{})
		return
	}
	rows, err := application.db.Query(request.Context(), `SELECT `+trackSelectColumns+`
		FROM tracks t LEFT JOIN favorites f ON f.track_id = t.id AND f.user_id = $1
		WHERE t.id = ANY($2)`, currentUser.ID, ids)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить треки")
		return
	}
	defer rows.Close()
	byID := map[int64]track{}
	for rows.Next() {
		var item track
		if err := scanTrack(rows, &item); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать треки")
			return
		}
		byID[item.ID] = item
	}
	items := make([]track, 0, len(ids))
	for _, id := range ids {
		if item, exists := byID[id]; exists {
			items = append(items, item)
		}
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) adminUsers(response http.ResponseWriter, request *http.Request, _ user) {
	rows, err := application.db.Query(request.Context(), "SELECT id, username, is_admin FROM users ORDER BY username")
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить пользователей")
		return
	}
	defer rows.Close()
	users := []user{}
	for rows.Next() {
		var item user
		if err := rows.Scan(&item.ID, &item.Username, &item.IsAdmin); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать пользователей")
			return
		}
		users = append(users, item)
	}
	writeJSON(response, http.StatusOK, users)
}

func (application *app) createUser(response http.ResponseWriter, request *http.Request, _ user) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"isAdmin"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Username = strings.TrimSpace(payload.Username)
	if !usernameValid(payload.Username) || !passwordValid(payload.Password) {
		writeError(response, http.StatusBadRequest, "Логин — от 3 символов, пароль — от 8")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), bcrypt.DefaultCost)
	var createdID int64
	if err == nil {
		err = application.db.QueryRow(request.Context(), "INSERT INTO users (username, password_hash, is_admin) VALUES ($1, $2, $3) RETURNING id", payload.Username, string(hash), payload.IsAdmin).Scan(&createdID)
	}
	if err != nil {
		writeError(response, http.StatusConflict, "Пользователь уже существует")
		return
	}
	writeJSON(response, http.StatusCreated, user{ID: createdID, Username: payload.Username, IsAdmin: payload.IsAdmin})
}

func (application *app) updateAccount(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		Username        string `json:"username"`
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Username = strings.TrimSpace(payload.Username)
	if !usernameValid(payload.Username) || !passwordValid(payload.CurrentPassword) || (payload.NewPassword != "" && !passwordValid(payload.NewPassword)) {
		writeError(response, http.StatusBadRequest, "Проверьте логин и пароль")
		return
	}
	var passwordHash string
	if err := application.db.QueryRow(request.Context(), "SELECT password_hash FROM users WHERE id = $1", currentUser.ID).Scan(&passwordHash); err != nil || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(payload.CurrentPassword)) != nil {
		writeError(response, http.StatusUnauthorized, "Неверный текущий пароль")
		return
	}
	if payload.NewPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(payload.NewPassword), bcrypt.DefaultCost)
		if err != nil {
			writeError(response, http.StatusBadRequest, "Некорректный новый пароль")
			return
		}
		passwordHash = string(hash)
	}
	token, err := randomToken()
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось обновить аккаунт")
		return
	}
	tx, err := application.db.Begin(request.Context())
	if err == nil {
		_, err = tx.Exec(request.Context(), "UPDATE users SET username = $1, password_hash = $2 WHERE id = $3", payload.Username, passwordHash, currentUser.ID)
	}
	if err == nil {
		_, err = tx.Exec(request.Context(), "DELETE FROM sessions WHERE user_id = $1", currentUser.ID)
	}
	if err == nil && payload.NewPassword != "" {
		_, err = tx.Exec(request.Context(), "DELETE FROM push_subscriptions WHERE user_id = $1", currentUser.ID)
	}
	if err == nil && payload.NewPassword != "" {
		_, err = tx.Exec(request.Context(), "DELETE FROM devices WHERE user_id = $1", currentUser.ID)
	}
	if err == nil {
		_, err = tx.Exec(request.Context(), "INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)", hashToken(token), currentUser.ID, time.Now().Add(30*24*time.Hour))
	}
	if err != nil || tx.Commit(request.Context()) != nil {
		if tx != nil {
			_ = tx.Rollback(request.Context())
		}
		writeError(response, http.StatusConflict, "Логин уже занят")
		return
	}
	setSessionCookie(response, request, token, 30*24*60*60)
	application.audit(request.Context(), currentUser.ID, "Изменены данные собственного аккаунта")
	writeJSON(response, http.StatusOK, map[string]string{"username": payload.Username})
}

func (application *app) updateUser(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	if id == currentUser.ID {
		writeError(response, http.StatusBadRequest, "Свой аккаунт изменяйте в настройках профиля")
		return
	}
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Username = strings.TrimSpace(payload.Username)
	if !usernameValid(payload.Username) || (payload.Password != "" && !passwordValid(payload.Password)) {
		writeError(response, http.StatusBadRequest, "Проверьте логин и пароль")
		return
	}
	passwordHash := ""
	if payload.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), bcrypt.DefaultCost)
		if err != nil {
			writeError(response, http.StatusBadRequest, "Некорректный пароль")
			return
		}
		passwordHash = string(hash)
	}
	tx, err := application.db.Begin(request.Context())
	var affected int64
	if err == nil {
		result, updateErr := tx.Exec(request.Context(), "UPDATE users SET username = $2, password_hash = CASE WHEN $3 = '' THEN password_hash ELSE $3 END WHERE id = $1", id, payload.Username, passwordHash)
		err = updateErr
		affected = result.RowsAffected()
	}
	if err == nil && affected > 0 {
		_, err = tx.Exec(request.Context(), "DELETE FROM sessions WHERE user_id = $1", id)
	}
	if err == nil && passwordHash != "" {
		_, err = tx.Exec(request.Context(), "DELETE FROM push_subscriptions WHERE user_id = $1", id)
	}
	if err == nil && passwordHash != "" {
		_, err = tx.Exec(request.Context(), "DELETE FROM devices WHERE user_id = $1", id)
	}
	if err != nil || affected == 0 || tx.Commit(request.Context()) != nil {
		if tx != nil {
			_ = tx.Rollback(request.Context())
		}
		writeError(response, http.StatusConflict, "Не удалось обновить пользователя")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) deleteUser(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil || id == currentUser.ID {
		writeError(response, http.StatusBadRequest, "Нельзя удалить текущего администратора")
		return
	}
	result, err := application.db.Exec(request.Context(), "DELETE FROM users WHERE id = $1", id)
	if err != nil || result.RowsAffected() == 0 {
		writeError(response, http.StatusNotFound, "Пользователь не найден")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) playlists(response http.ResponseWriter, request *http.Request, _ user) {
	rows, err := application.db.Query(request.Context(), `
		SELECT p.id, p.name, p.description, u.username,
			'edit',
			COUNT(pt.track_id), COALESCE(ARRAY_AGG(pt.track_id ORDER BY pt.position) FILTER (WHERE pt.track_id IS NOT NULL), ARRAY[]::BIGINT[]),
			COALESCE(OCTET_LENGTH(p.cover), 0) > 0
		FROM playlists p JOIN users u ON u.id = p.owner_id
		LEFT JOIN playlist_tracks pt ON pt.playlist_id = p.id
		GROUP BY p.id, u.username ORDER BY p.name`)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить плейлисты")
		return
	}
	defer rows.Close()
	items := []playlist{}
	for rows.Next() {
		var item playlist
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Owner, &item.Permission, &item.TrackCount, &item.TrackIDs, &item.HasCover); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать плейлисты")
			return
		}
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) playlistCover(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok || !application.playlistAllowed(request, currentUser, id, "view") {
		writeError(response, http.StatusForbidden, "Обложка недоступна")
		return
	}
	var data []byte
	var contentType string
	if err := application.db.QueryRow(request.Context(), "SELECT cover, cover_mime FROM playlists WHERE id = $1", id).Scan(&data, &contentType); err != nil || len(data) == 0 {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "private, no-cache")
	_, _ = response.Write(data)
}

func (application *app) updatePlaylistCover(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	if !application.playlistAllowed(request, currentUser, id, "edit") {
		writeError(response, http.StatusForbidden, "Плейлист недоступен")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 5<<20)
	file, _, err := request.FormFile("cover")
	if err != nil {
		writeError(response, http.StatusBadRequest, "Выберите изображение")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	contentType, valid := playlistCoverValid(data)
	if err != nil || !valid {
		writeError(response, http.StatusBadRequest, "Нужна квадратная JPEG или PNG от 128 до 2048 px")
		return
	}
	if _, err = application.db.Exec(request.Context(), "UPDATE playlists SET cover = $1, cover_mime = $2 WHERE id = $3", data, contentType, id); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось сохранить обложку")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func playlistCoverValid(data []byte) (string, bool) {
	contentType := http.DetectContentType(data)
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	return contentType, err == nil && (contentType == "image/jpeg" || contentType == "image/png") && config.Width == config.Height && config.Width >= 128 && config.Width <= 2048
}

func (application *app) createPlaylist(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	payload.Description = strings.TrimSpace(payload.Description)
	if !playlistDetailsValid(payload.Name, payload.Description) {
		writeError(response, http.StatusBadRequest, "Название — до 100 символов, описание — до 500")
		return
	}
	var item playlist
	err := application.db.QueryRow(request.Context(), "INSERT INTO playlists (owner_id, name, description) VALUES ($1, $2, $3) RETURNING id, name, description", currentUser.ID, payload.Name, payload.Description).Scan(&item.ID, &item.Name, &item.Description)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось создать плейлист")
		return
	}
	item.Owner, item.Permission = currentUser.Username, "edit"
	writeJSON(response, http.StatusCreated, item)
}

func (application *app) updatePlaylist(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok || !application.playlistAllowed(request, currentUser, id, "edit") {
		writeError(response, http.StatusForbidden, "Плейлист недоступен")
		return
	}
	var payload struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	payload.Description = strings.TrimSpace(payload.Description)
	if !playlistDetailsValid(payload.Name, payload.Description) {
		writeError(response, http.StatusBadRequest, "Название — до 100 символов, описание — до 500")
		return
	}
	result, err := application.db.Exec(request.Context(), "UPDATE playlists SET name = $1, description = $2 WHERE id = $3", payload.Name, payload.Description, id)
	if err != nil || result.RowsAffected() == 0 {
		writeError(response, http.StatusNotFound, "Плейлист не найден")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func playlistDetailsValid(name, description string) bool {
	return name != "" && len([]rune(name)) <= 100 && len([]rune(description)) <= 500
}

func (application *app) playlistTracks(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok || !application.playlistAllowed(request, currentUser, id, "view") {
		writeError(response, http.StatusForbidden, "Плейлист недоступен")
		return
	}
	rows, err := application.db.Query(request.Context(), `SELECT `+trackSelectColumns+`
		FROM playlist_tracks pt JOIN tracks t ON t.id = pt.track_id
		LEFT JOIN favorites f ON f.track_id = t.id AND f.user_id = $1
		WHERE pt.playlist_id = $2 ORDER BY pt.position`, currentUser.ID, id)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить плейлист")
		return
	}
	defer rows.Close()
	items := []track{}
	for rows.Next() {
		var item track
		if err := scanTrack(rows, &item); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать плейлист")
			return
		}
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) deletePlaylist(response http.ResponseWriter, request *http.Request, _ user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	result, err := application.db.Exec(request.Context(), "DELETE FROM playlists WHERE id = $1", id)
	if err != nil || result.RowsAffected() == 0 {
		writeError(response, http.StatusNotFound, "Плейлист не найден")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) addPlaylistTrack(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok || !application.playlistAllowed(request, currentUser, id, "add") {
		writeError(response, http.StatusForbidden, "Нельзя изменять этот плейлист")
		return
	}
	var payload struct {
		TrackID  int64   `json:"trackId"`
		TrackIDs []int64 `json:"trackIds"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	ids := payload.TrackIDs
	if len(ids) == 0 && payload.TrackID != 0 {
		ids = []int64{payload.TrackID}
	}
	if len(ids) == 0 {
		writeError(response, http.StatusBadRequest, "Не указаны треки")
		return
	}
	tx, err := application.db.Begin(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Ошибка сохранения")
		return
	}
	defer tx.Rollback(request.Context())
	var maxPosition int
	_ = tx.QueryRow(request.Context(), "SELECT COALESCE(MAX(position), -1) FROM playlist_tracks WHERE playlist_id = $1", id).Scan(&maxPosition)
	for _, trackID := range ids {
		maxPosition++
		_, err = tx.Exec(request.Context(), `
			INSERT INTO playlist_tracks (playlist_id, track_id, position, added_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, id, trackID, maxPosition, currentUser.ID)
		if err != nil {
			writeError(response, http.StatusBadRequest, "Не удалось добавить треки")
			return
		}
	}
	if err := tx.Commit(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Ошибка сохранения")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) removePlaylistTrack(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	trackID, trackOK := parseID(response, request.PathValue("trackId"))
	if !ok || !trackOK || !application.playlistAllowed(request, currentUser, id, "edit") {
		writeError(response, http.StatusForbidden, "Нельзя изменять этот плейлист")
		return
	}
	_, _ = application.db.Exec(request.Context(), "DELETE FROM playlist_tracks WHERE playlist_id = $1 AND track_id = $2", id, trackID)
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) reorderPlaylistTracks(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok || !application.playlistAllowed(request, currentUser, id, "edit") {
		writeError(response, http.StatusForbidden, "Нельзя изменять этот плейлист")
		return
	}
	var payload struct {
		TrackIDs []int64 `json:"trackIds"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	tx, err := application.db.Begin(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Ошибка базы данных")
		return
	}
	defer tx.Rollback(request.Context())
	for index, trackID := range payload.TrackIDs {
		if _, err := tx.Exec(request.Context(), "UPDATE playlist_tracks SET position = $1 WHERE playlist_id = $2 AND track_id = $3", index+1, id, trackID); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось изменить порядок")
			return
		}
	}
	if err := tx.Commit(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось сохранить порядок")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) playlistAllowed(request *http.Request, _ user, id int64, _ string) bool {
	var exists bool
	err := application.db.QueryRow(request.Context(), "SELECT EXISTS(SELECT 1 FROM playlists WHERE id = $1)", id).Scan(&exists)
	return err == nil && exists
}

func (application *app) history(response http.ResponseWriter, request *http.Request, currentUser user) {
	rows, err := application.db.Query(request.Context(), `SELECT `+trackSelectColumns+`
		FROM (SELECT DISTINCT ON (track_id) track_id, listened_at FROM listening_history WHERE user_id = $1 ORDER BY track_id, listened_at DESC) h
		JOIN tracks t ON t.id = h.track_id LEFT JOIN favorites f ON f.track_id = t.id AND f.user_id = $1
		ORDER BY h.listened_at DESC LIMIT 100`, currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить историю")
		return
	}
	defer rows.Close()
	items := []track{}
	for rows.Next() {
		var item track
		if err := scanTrack(rows, &item); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать историю")
			return
		}
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) addHistory(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		TrackID  int64   `json:"trackId"`
		Position float64 `json:"position"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	_, err := application.db.Exec(request.Context(), "INSERT INTO listening_history (user_id, track_id, position) VALUES ($1, $2, $3)", currentUser.ID, payload.TrackID, payload.Position)
	if err != nil {
		writeError(response, http.StatusBadRequest, "Не удалось сохранить историю")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) playbackState(response http.ResponseWriter, request *http.Request, currentUser user) {
	var trackID *int64
	var position float64
	var queue []byte
	var isPlaying bool
	err := application.db.QueryRow(request.Context(), "SELECT current_track_id, position, queue, is_playing FROM playback_states WHERE user_id = $1", currentUser.ID).Scan(&trackID, &position, &queue, &isPlaying)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(response, http.StatusOK, map[string]any{"currentTrackId": nil, "position": 0, "queue": []int64{}, "isPlaying": false})
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить состояние")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"currentTrackId": trackID, "position": position, "queue": jsonRaw(queue), "isPlaying": isPlaying})
}

func (application *app) savePlaybackState(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		CurrentTrackID *int64  `json:"currentTrackId"`
		Position       float64 `json:"position"`
		Queue          []int64 `json:"queue"`
		IsPlaying      bool    `json:"isPlaying"`
	}
	if !decodeJSON(response, request, &payload) || len(payload.Queue) > 1000 {
		return
	}
	queue, _ := json.Marshal(payload.Queue)
	_, err := application.db.Exec(request.Context(), `
		INSERT INTO playback_states (user_id, current_track_id, position, queue, is_playing) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET current_track_id = EXCLUDED.current_track_id,
			position = EXCLUDED.position, queue = EXCLUDED.queue, is_playing = EXCLUDED.is_playing,
			updated_at = NOW()`, currentUser.ID, payload.CurrentTrackID, payload.Position, string(queue), payload.IsPlaying)
	if err != nil {
		writeError(response, http.StatusBadRequest, "Не удалось сохранить состояние")
		return
	}
	application.devices.broadcast(currentUser.ID, deviceEvent{Type: "state"})
	response.WriteHeader(http.StatusNoContent)
}

func parseID(response http.ResponseWriter, value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		writeError(response, http.StatusBadRequest, "Некорректный идентификатор")
		return 0, false
	}
	return id, true
}

type jsonRaw []byte

func (value jsonRaw) MarshalJSON() ([]byte, error) {
	return value, nil
}
