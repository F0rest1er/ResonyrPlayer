package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/ergochat/webpush-go/v2"
	"github.com/jackc/pgx/v5"
)

type notification struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

func (application *app) loadPushKeys(ctx context.Context) error {
	var data string
	err := application.db.QueryRow(ctx, "SELECT value FROM server_settings WHERE key = 'vapid_keys'").Scan(&data)
	if err == nil {
		application.vapidKeys = new(webpush.VAPIDKeys)
		return json.Unmarshal([]byte(data), application.vapidKeys)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	keys, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(keys)
	if err == nil {
		_, err = application.db.Exec(ctx, "INSERT INTO server_settings (key, value) VALUES ('vapid_keys', $1)", string(encoded))
	}
	application.vapidKeys = keys
	return err
}

func (application *app) notifications(response http.ResponseWriter, request *http.Request, currentUser user) {
	rows, err := application.db.Query(request.Context(), "SELECT id, title, body, created_at FROM notifications WHERE user_id = $1 ORDER BY created_at DESC LIMIT 100", currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить уведомления")
		return
	}
	defer rows.Close()
	items := []notification{}
	for rows.Next() {
		var item notification
		var createdAt time.Time
		if err := rows.Scan(&item.ID, &item.Title, &item.Body, &createdAt); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать уведомления")
			return
		}
		item.CreatedAt = createdAt.Format(time.RFC3339)
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) clearNotifications(response http.ResponseWriter, request *http.Request, currentUser user) {
	if _, err := application.db.Exec(request.Context(), "DELETE FROM notifications WHERE user_id = $1", currentUser.ID); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось очистить уведомления")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) pushKey(response http.ResponseWriter, _ *http.Request, _ user) {
	writeJSON(response, http.StatusOK, map[string]string{"publicKey": application.vapidKeys.PublicKeyString()})
}

func (application *app) savePushSubscription(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		Endpoint       string `json:"endpoint"`
		ExpirationTime *int64 `json:"expirationTime"`
		Keys           struct {
			Auth   string `json:"auth"`
			P256dh string `json:"p256dh"`
		} `json:"keys"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	keys, err := webpush.DecodeSubscriptionKeys(payload.Keys.Auth, payload.Keys.P256dh)
	if err != nil || !pushEndpointValid(payload.Endpoint) {
		writeError(response, http.StatusBadRequest, "Некорректная push-подписка")
		return
	}
	subscription := webpush.Subscription{Endpoint: payload.Endpoint, Keys: keys}
	data, err := json.Marshal(subscription)
	if err == nil {
		_, err = application.db.Exec(request.Context(), `INSERT INTO push_subscriptions (user_id, endpoint, subscription) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, endpoint) DO UPDATE SET subscription = EXCLUDED.subscription`, currentUser.ID, subscription.Endpoint, string(data))
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось сохранить push-подписку")
		return
	}
	application.notifyUser(request.Context(), currentUser.ID, "Push включён", "Уведомления будут приходить на это устройство")
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) deletePushSubscription(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if !pushEndpointValid(payload.Endpoint) {
		writeError(response, http.StatusBadRequest, "Некорректная push-подписка")
		return
	}
	_, _ = application.db.Exec(request.Context(), "DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2", currentUser.ID, payload.Endpoint)
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) notifyUser(ctx context.Context, userID int64, title, body string) {
	if len(title) > 120 || len(body) > 500 {
		return
	}
	_, _ = application.db.Exec(ctx, "INSERT INTO notifications (user_id, title, body) VALUES ($1, $2, $3)", userID, title, body)
	rows, err := application.db.Query(ctx, "SELECT endpoint, subscription FROM push_subscriptions WHERE user_id = $1", userID)
	if err != nil {
		return
	}
	defer rows.Close()
	payload, _ := json.Marshal(map[string]string{"title": title, "body": body, "url": "/"})
	for rows.Next() {
		var endpoint string
		var data []byte
		var subscription webpush.Subscription
		if rows.Scan(&endpoint, &data) != nil || json.Unmarshal(data, &subscription) != nil {
			continue
		}
		pushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		pushResponse, sendErr := webpush.SendNotification(pushCtx, payload, &subscription, &webpush.Options{HTTPClient: pushHTTPClient, Subscriber: env("PUBLIC_URL", "https://localhost"), VAPIDKeys: application.vapidKeys, TTL: 86400})
		cancel()
		if pushResponse != nil {
			pushResponse.Body.Close()
			if pushResponse.StatusCode == http.StatusGone || pushResponse.StatusCode == http.StatusNotFound {
				_, _ = application.db.Exec(ctx, "DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2", userID, endpoint)
			}
		}
		if sendErr != nil {
			continue
		}
	}
}

var pushHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (application *app) notifyAll(ctx context.Context, title, body string) {
	rows, err := application.db.Query(ctx, "SELECT id FROM users")
	if err != nil {
		return
	}
	defer rows.Close()
	userIDs := []int64{}
	for rows.Next() {
		var userID int64
		if rows.Scan(&userID) == nil {
			userIDs = append(userIDs, userID)
		}
	}
	for _, userID := range userIDs {
		application.notifyUser(ctx, userID, title, body)
	}
}

func pushEndpointValid(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{"fcm.googleapis.com", "push.services.mozilla.com", "notify.windows.com", "push.apple.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
