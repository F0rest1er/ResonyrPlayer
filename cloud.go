package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type musicSource struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	RemotePath string `json:"remotePath"`
	LastSyncAt string `json:"lastSyncAt"`
	LastError  string `json:"lastError"`
}

type sourceRequest struct {
	AccessToken  string `json:"accessToken"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	RemotePath   string `json:"remotePath"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type cloudToken struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	TokenType    string    `json:"tokenType"`
	Expiry       time.Time `json:"expiry"`
}

type cloudManifestEntry struct {
	Key     string `json:"key"`
	Version string `json:"version"`
}

type cloudManifest struct {
	Files map[string]cloudManifestEntry `json:"files"`
}

type cloudSync struct {
	current     map[string]cloudManifestEntry
	destination string
	old         map[string]cloudManifestEntry
	oldByKey    map[string]string
	source      string
}

type oauthProvider struct {
	AuthURL      string
	ClientID     string
	ClientSecret string
	Scope        string
	TokenURL     string
}

type oauthCredentials struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

var cloudHTTPClient = &http.Client{Timeout: 30 * time.Minute}

func cloudProviderSpec(name string) (oauthProvider, bool) {
	providers := map[string]oauthProvider{
		"google-drive": {AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", Scope: "https://www.googleapis.com/auth/drive.readonly"},
		"yandex-disk":  {AuthURL: "https://oauth.yandex.ru/authorize", TokenURL: "https://oauth.yandex.ru/token"},
		"dropbox":      {AuthURL: "https://www.dropbox.com/oauth2/authorize", TokenURL: "https://api.dropboxapi.com/oauth2/token"},
		"onedrive":     {AuthURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize", TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token", Scope: "offline_access Files.Read"},
	}
	provider, ok := providers[name]
	return provider, ok
}

func (application *app) cloudProvider(ctx context.Context, name string) (oauthProvider, bool) {
	provider, ok := cloudProviderSpec(name)
	if !ok {
		return provider, false
	}
	var encrypted []byte
	if err := application.db.QueryRow(ctx, "SELECT config FROM oauth_credentials WHERE provider = $1", name).Scan(&encrypted); err == nil {
		if data, err := application.decryptConfig(encrypted); err == nil {
			var credentials oauthCredentials
			if json.Unmarshal(data, &credentials) == nil {
				provider.ClientID = credentials.ClientID
				provider.ClientSecret = credentials.ClientSecret
			}
		}
	}
	if provider.ClientID == "" {
		prefixes := map[string]string{"google-drive": "GOOGLE", "yandex-disk": "YANDEX", "dropbox": "DROPBOX", "onedrive": "ONEDRIVE"}
		provider.ClientID = os.Getenv(prefixes[name] + "_CLIENT_ID")
		provider.ClientSecret = os.Getenv(prefixes[name] + "_CLIENT_SECRET")
	}
	return provider, true
}

func (application *app) saveOAuthCredentials(ctx context.Context, provider string, credentials oauthCredentials) error {
	data, err := json.Marshal(credentials)
	if err == nil {
		data, err = application.encryptConfig(data)
	}
	if err == nil {
		_, err = application.db.Exec(ctx, "INSERT INTO oauth_credentials (provider, config) VALUES ($1, $2) ON CONFLICT (provider) DO UPDATE SET config = EXCLUDED.config", provider, data)
	}
	return err
}

func (application *app) cleanupLegacySources(ctx context.Context) error {
	rows, err := application.db.Query(ctx, "DELETE FROM music_sources WHERE remote_name <> 'native' RETURNING id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			_ = os.RemoveAll(application.sourceDir(id))
		}
	}
	return rows.Err()
}

func (application *app) sources(response http.ResponseWriter, request *http.Request, _ user) {
	rows, err := application.db.Query(request.Context(), "SELECT id, name, provider, remote_path, last_sync_at, last_error FROM music_sources WHERE remote_name = 'native' AND provider = ANY($1) ORDER BY name", []string{"google-drive", "yandex-disk", "dropbox", "onedrive"})
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить источники")
		return
	}
	defer rows.Close()
	items := []musicSource{}
	for rows.Next() {
		var item musicSource
		var lastSyncAt *time.Time
		if rows.Scan(&item.ID, &item.Name, &item.Provider, &item.RemotePath, &lastSyncAt, &item.LastError) != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать источники")
			return
		}
		if lastSyncAt != nil {
			item.LastSyncAt = lastSyncAt.Format(time.RFC3339)
		}
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) startSourceOAuth(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload sourceRequest
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.Name = strings.TrimSpace(payload.Name)
	payload.Provider = strings.TrimSpace(payload.Provider)
	payload.RemotePath = strings.Trim(strings.TrimSpace(payload.RemotePath), "/")
	payload.ClientID = strings.TrimSpace(payload.ClientID)
	payload.ClientSecret = strings.TrimSpace(payload.ClientSecret)
	payload.AccessToken = strings.TrimSpace(payload.AccessToken)
	_, ok := cloudProviderSpec(payload.Provider)
	if !ok || !sourceRequestValid(payload) {
		writeError(response, http.StatusBadRequest, "Некорректный источник")
		return
	}
	if payload.AccessToken != "" {
		if payload.Provider != "yandex-disk" || len(payload.AccessToken) > 4096 || strings.ContainsAny(payload.AccessToken, " \r\n\t") {
			writeError(response, http.StatusBadRequest, "Некорректный токен Яндекс Диска")
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
		defer cancel()
		var diskInfo map[string]any
		if err := cloudGETWithScheme(ctx, "https://cloud-api.yandex.net/v1/disk/resources?path=disk%3A%2F&limit=1", "OAuth", payload.AccessToken, &diskInfo); err != nil {
			writeError(response, http.StatusBadRequest, "Не удалось проверить токен Яндекс Диска. Проверьте право чтения и доступность сервиса")
			return
		}
		config, err := json.Marshal(cloudToken{AccessToken: payload.AccessToken, TokenType: "OAuth"})
		if err == nil {
			config, err = application.encryptConfig(config)
		}
		if err == nil {
			_, err = application.db.Exec(request.Context(), "INSERT INTO music_sources (name, provider, remote_name, remote_path, config) VALUES ($1, $2, 'native', $3, $4)", payload.Name, payload.Provider, payload.RemotePath, config)
		}
		if err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось сохранить источник")
			return
		}
		writeJSON(response, http.StatusOK, map[string]string{"url": "/?source=connected"})
		return
	}
	publicURL := strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/")
	if origin := request.Header.Get("Origin"); origin != "" && origin != publicURL {
		writeError(response, http.StatusBadRequest, "Откройте плеер по адресу PUBLIC_URL из .env. Адрес входа и возврата из облака должен совпадать")
		return
	}
	if (payload.ClientID == "") != (payload.ClientSecret == "") || !oauthCredentialsValid(oauthCredentials{ClientID: payload.ClientID, ClientSecret: payload.ClientSecret}, payload.ClientID == "") {
		writeError(response, http.StatusBadRequest, "Укажите Client ID и Client Secret полностью")
		return
	}
	if payload.ClientID != "" {
		if err := application.saveOAuthCredentials(request.Context(), payload.Provider, oauthCredentials{ClientID: payload.ClientID, ClientSecret: payload.ClientSecret}); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось сохранить OAuth-настройки")
			return
		}
	}
	provider, _ := application.cloudProvider(request.Context(), payload.Provider)
	if provider.ClientID == "" || provider.ClientSecret == "" {
		writeError(response, http.StatusPreconditionFailed, "Укажите Client ID и Client Secret провайдера")
		return
	}
	state, err := randomToken()
	if err == nil {
		_, _ = application.db.Exec(request.Context(), "DELETE FROM oauth_states WHERE expires_at <= NOW()")
		_, err = application.db.Exec(request.Context(), "INSERT INTO oauth_states (state_hash, user_id, provider, name, remote_path, expires_at) VALUES ($1, $2, $3, $4, $5, NOW() + INTERVAL '10 minutes')", hashToken(state), currentUser.ID, payload.Provider, payload.Name, payload.RemotePath)
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось начать подключение")
		return
	}
	callback := strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/") + "/api/admin/sources/oauth/callback"
	query := url.Values{"client_id": {provider.ClientID}, "redirect_uri": {callback}, "response_type": {"code"}, "state": {state}}
	if provider.Scope != "" {
		query.Set("scope", provider.Scope)
	}
	switch payload.Provider {
	case "google-drive":
		query.Set("access_type", "offline")
		query.Set("prompt", "consent")
	case "dropbox":
		query.Set("token_access_type", "offline")
	case "yandex-disk":
		query.Set("force_confirm", "yes")
	}
	writeJSON(response, http.StatusOK, map[string]string{"url": provider.AuthURL + "?" + query.Encode()})
}

func (application *app) sourceOAuthCallback(response http.ResponseWriter, request *http.Request, currentUser user) {
	state := request.URL.Query().Get("state")
	code := request.URL.Query().Get("code")
	if state == "" || code == "" || request.URL.Query().Get("error") != "" {
		http.Redirect(response, request, "/?source=error", http.StatusSeeOther)
		return
	}
	var payload sourceRequest
	err := application.db.QueryRow(request.Context(), "DELETE FROM oauth_states WHERE state_hash = $1 AND user_id = $2 AND expires_at > NOW() RETURNING name, provider, remote_path", hashToken(state), currentUser.ID).Scan(&payload.Name, &payload.Provider, &payload.RemotePath)
	if err != nil {
		http.Redirect(response, request, "/?source=expired", http.StatusSeeOther)
		return
	}
	provider, ok := application.cloudProvider(request.Context(), payload.Provider)
	if !ok {
		http.Redirect(response, request, "/?source=error", http.StatusSeeOther)
		return
	}
	callback := strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/") + "/api/admin/sources/oauth/callback"
	token, err := exchangeOAuthCode(request.Context(), provider, code, callback)
	if err != nil {
		http.Redirect(response, request, "/?source=error", http.StatusSeeOther)
		return
	}
	config, err := json.Marshal(token)
	if err == nil {
		config, err = application.encryptConfig(config)
	}
	if err == nil {
		_, err = application.db.Exec(request.Context(), "INSERT INTO music_sources (name, provider, remote_name, remote_path, config) VALUES ($1, $2, 'native', $3, $4)", payload.Name, payload.Provider, payload.RemotePath, config)
	}
	if err != nil {
		http.Redirect(response, request, "/?source=error", http.StatusSeeOther)
		return
	}
	http.Redirect(response, request, "/?source=connected", http.StatusSeeOther)
}

func exchangeOAuthCode(ctx context.Context, provider oauthProvider, code, callback string) (cloudToken, error) {
	values := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {provider.ClientID}, "client_secret": {provider.ClientSecret}, "redirect_uri": {callback}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := cloudJSON(request, &response); err != nil || response.AccessToken == "" {
		return cloudToken{}, errors.New("OAuth token exchange failed")
	}
	token := cloudToken{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, TokenType: response.TokenType}
	if response.ExpiresIn > 0 {
		token.Expiry = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	return token, nil
}

func (application *app) deleteSource(response http.ResponseWriter, request *http.Request, _ user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	result, err := application.db.Exec(request.Context(), "DELETE FROM music_sources WHERE id = $1", id)
	if err != nil || result.RowsAffected() == 0 {
		writeError(response, http.StatusNotFound, "Источник не найден")
		return
	}
	_ = os.RemoveAll(application.sourceDir(id))
	if err := application.scan(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Источник удалён, но библиотека не обновлена")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) syncSourceHandler(response http.ResponseWriter, request *http.Request, _ user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	if err := application.syncSource(request.Context(), id); err != nil {
		writeError(response, http.StatusBadGateway, "Синхронизация источника завершилась с ошибкой")
		return
	}
	if err := application.scan(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Источник синхронизирован, но медиатека не обновлена")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) syncSources(ctx context.Context) error {
	rows, err := application.db.Query(ctx, "SELECT id FROM music_sources WHERE remote_name = 'native' AND provider = ANY($1)", []string{"google-drive", "yandex-disk", "dropbox", "onedrive"})
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	errorsFound := []error{}
	for _, id := range ids {
		if err := application.syncSource(ctx, id); err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("источник %d: %w", id, err))
		}
	}
	return errors.Join(errorsFound...)
}

func (application *app) syncSource(ctx context.Context, id int64) error {
	var providerName, remotePath string
	var encrypted []byte
	if err := application.db.QueryRow(ctx, "SELECT provider, remote_path, config FROM music_sources WHERE id = $1", id).Scan(&providerName, &remotePath, &encrypted); err != nil {
		return err
	}
	config, err := application.decryptConfig(encrypted)
	var token cloudToken
	if err == nil {
		err = json.Unmarshal(config, &token)
	}
	if err == nil {
		token, err = application.refreshCloudToken(ctx, id, providerName, token)
	}
	parent := filepath.Join(application.dataDir, "cloud")
	if err == nil {
		err = os.MkdirAll(parent, 0o750)
	}
	staging := ""
	if err == nil {
		staging, err = os.MkdirTemp(parent, ".sync-*")
	}
	if staging != "" {
		defer os.RemoveAll(staging)
	}
	destination := application.sourceDir(id)
	syncer := newCloudSync(destination, staging)
	if err == nil {
		switch providerName {
		case "google-drive":
			err = syncGoogleDrive(ctx, token.AccessToken, remotePath, syncer)
		case "yandex-disk":
			err = syncYandexDisk(ctx, token.AccessToken, remotePath, syncer)
		case "dropbox":
			err = syncDropbox(ctx, token.AccessToken, remotePath, syncer)
		case "onedrive":
			err = syncOneDrive(ctx, token.AccessToken, remotePath, syncer)
		default:
			err = errors.New("unsupported cloud provider")
		}
	}
	if err == nil {
		err = syncer.writeManifest()
	}
	if err == nil {
		err = replaceCloudDirectory(destination, staging)
		if err == nil {
			staging = ""
		}
	}
	message := ""
	if err != nil {
		message = err.Error()
		if len(message) > 1000 {
			message = message[:1000]
		}
	}
	_, _ = application.db.Exec(ctx, "UPDATE music_sources SET last_sync_at = NOW(), last_error = $2 WHERE id = $1", id, message)
	return err
}

func newCloudSync(source, destination string) *cloudSync {
	syncer := &cloudSync{source: source, destination: destination, old: map[string]cloudManifestEntry{}, oldByKey: map[string]string{}, current: map[string]cloudManifestEntry{}}
	data, err := os.ReadFile(filepath.Join(source, ".resonyr-manifest.json"))
	var manifest cloudManifest
	if err == nil && json.Unmarshal(data, &manifest) == nil {
		syncer.old = manifest.Files
		for path, entry := range manifest.Files {
			if entry.Key != "" {
				syncer.oldByKey[entry.Key] = path
			}
		}
	}
	return syncer
}

func (syncer *cloudSync) add(key, version, relative string, download func(string) error) error {
	relative = filepath.ToSlash(relative)
	if _, ok := safeCloudRelativePath(relative); !ok {
		return errors.New("invalid cloud file path")
	}
	target := filepath.Join(syncer.destination, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	if oldRelative, ok := syncer.oldByKey[key]; ok && key != "" && version != "" && syncer.old[oldRelative].Version == version {
		if err := linkOrCopyCloudFile(filepath.Join(syncer.source, filepath.FromSlash(oldRelative)), target); err == nil {
			syncer.current[relative] = cloudManifestEntry{Key: key, Version: version}
			return nil
		}
	}
	if err := download(target); err != nil {
		return err
	}
	syncer.current[relative] = cloudManifestEntry{Key: key, Version: version}
	return nil
}

func (syncer *cloudSync) writeManifest() error {
	data, err := json.Marshal(cloudManifest{Files: syncer.current})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(syncer.destination, ".resonyr-manifest.json"), data, 0o600)
}

func linkOrCopyCloudFile(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("cached cloud file is unavailable")
	}
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}

func replaceCloudDirectory(destination, staging string) error {
	backup := destination + ".previous"
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	hadDestination := true
	if err := os.Rename(destination, backup); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		hadDestination = false
	}
	if err := os.Rename(staging, destination); err != nil {
		if hadDestination {
			_ = os.Rename(backup, destination)
		}
		return err
	}
	if hadDestination {
		return os.RemoveAll(backup)
	}
	return nil
}

func (application *app) refreshCloudToken(ctx context.Context, id int64, providerName string, token cloudToken) (cloudToken, error) {
	if token.AccessToken == "" {
		return token, errors.New("empty cloud access token")
	}
	if token.Expiry.IsZero() || time.Until(token.Expiry) > 2*time.Minute {
		return token, nil
	}
	provider, ok := application.cloudProvider(ctx, providerName)
	if !ok || token.RefreshToken == "" {
		return token, errors.New("cloud token expired")
	}
	values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "client_id": {provider.ClientID}, "client_secret": {provider.ClientSecret}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, provider.TokenURL, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := cloudJSON(request, &refreshed); err != nil || refreshed.AccessToken == "" {
		return token, errors.New("cloud token refresh failed")
	}
	token.AccessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		token.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.TokenType != "" {
		token.TokenType = refreshed.TokenType
	}
	if refreshed.ExpiresIn > 0 {
		token.Expiry = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	}
	data, _ := json.Marshal(token)
	encrypted, err := application.encryptConfig(data)
	if err == nil {
		_, err = application.db.Exec(ctx, "UPDATE music_sources SET config = $2 WHERE id = $1", id, encrypted)
	}
	return token, err
}

func syncGoogleDrive(ctx context.Context, accessToken, remotePath string, syncer *cloudSync) error {
	parentID := "root"
	for _, part := range splitCloudPath(remotePath) {
		query := fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = 'application/vnd.google-apps.folder' and trashed = false", googleEscape(parentID), googleEscape(part))
		var result struct {
			Files []struct {
				ID string `json:"id"`
			} `json:"files"`
		}
		endpoint := "https://www.googleapis.com/drive/v3/files?q=" + url.QueryEscape(query) + "&fields=files(id)&pageSize=2"
		if err := cloudGET(ctx, endpoint, accessToken, &result); err != nil || len(result.Files) != 1 {
			return errors.New("Google Drive folder not found")
		}
		parentID = result.Files[0].ID
	}
	var walk func(string, string) error
	walk = func(folderID, relative string) error {
		pageToken := ""
		for {
			query := fmt.Sprintf("'%s' in parents and trashed = false", googleEscape(folderID))
			values := url.Values{"q": {query}, "fields": {"nextPageToken,files(id,name,mimeType,md5Checksum,modifiedTime,size)"}, "pageSize": {"1000"}}
			if pageToken != "" {
				values.Set("pageToken", pageToken)
			}
			var result struct {
				NextPageToken string `json:"nextPageToken"`
				Files         []struct {
					ID           string `json:"id"`
					Name         string `json:"name"`
					MimeType     string `json:"mimeType"`
					MD5Checksum  string `json:"md5Checksum"`
					ModifiedTime string `json:"modifiedTime"`
					Size         int64  `json:"size,string"`
				} `json:"files"`
			}
			if err := cloudGET(ctx, "https://www.googleapis.com/drive/v3/files?"+values.Encode(), accessToken, &result); err != nil {
				return err
			}
			for _, item := range result.Files {
				name, ok := safeCloudName(item.Name)
				if !ok {
					continue
				}
				path := filepath.Join(relative, name)
				if item.MimeType == "application/vnd.google-apps.folder" {
					if err := walk(item.ID, path); err != nil {
						return err
					}
				} else if audioExtensions[strings.ToLower(filepath.Ext(name))] {
					version := cloudVersion(item.MD5Checksum, item.ModifiedTime, item.Size)
					if err := syncer.add("google:"+item.ID, version, path, func(target string) error {
						return downloadCloudFile(ctx, "https://www.googleapis.com/drive/v3/files/"+url.PathEscape(item.ID)+"?alt=media", accessToken, nil, target)
					}); err != nil {
						return err
					}
				}
			}
			pageToken = result.NextPageToken
			if pageToken == "" {
				return nil
			}
		}
	}
	return walk(parentID, "")
}

func syncYandexDisk(ctx context.Context, accessToken, remotePath string, syncer *cloudSync) error {
	var walk func(string, string) error
	walk = func(cloudPath, relative string) error {
		offset := 0
		for {
			values := url.Values{"path": {cloudPath}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}, "fields": {"_embedded.items.name,_embedded.items.path,_embedded.items.type,_embedded.items.file,_embedded.items.resource_id,_embedded.items.md5,_embedded.items.modified,_embedded.items.size,_embedded.total"}}
			var result struct {
				Embedded struct {
					Items []struct {
						Name       string `json:"name"`
						Path       string `json:"path"`
						Type       string `json:"type"`
						File       string `json:"file"`
						ResourceID string `json:"resource_id"`
						MD5        string `json:"md5"`
						Modified   string `json:"modified"`
						Size       int64  `json:"size"`
					} `json:"items"`
					Total int `json:"total"`
				} `json:"_embedded"`
			}
			if err := cloudGETWithScheme(ctx, "https://cloud-api.yandex.net/v1/disk/resources?"+values.Encode(), "OAuth", accessToken, &result); err != nil {
				return err
			}
			for _, item := range result.Embedded.Items {
				name, ok := safeCloudName(item.Name)
				if !ok {
					continue
				}
				path := filepath.Join(relative, name)
				if item.Type == "dir" {
					if err := walk(item.Path, path); err != nil {
						return err
					}
				} else if audioExtensions[strings.ToLower(filepath.Ext(name))] && item.File != "" {
					key := item.ResourceID
					if key == "" {
						key = item.Path
					}
					version := cloudVersion(item.MD5, item.Modified, item.Size)
					if err := syncer.add("yandex:"+key, version, path, func(target string) error {
						return downloadCloudFile(ctx, item.File, "", nil, target)
					}); err != nil {
						return err
					}
				}
			}
			offset += len(result.Embedded.Items)
			if offset >= result.Embedded.Total || len(result.Embedded.Items) == 0 {
				return nil
			}
		}
	}
	path := "disk:/" + strings.Trim(remotePath, "/")
	if remotePath == "" {
		path = "disk:/"
	}
	return walk(path, "")
}

func syncDropbox(ctx context.Context, accessToken, remotePath string, syncer *cloudSync) error {
	endpoint := "https://api.dropboxapi.com/2/files/list_folder"
	body := map[string]any{"path": "", "recursive": true, "include_deleted": false, "limit": 2000}
	root := strings.Trim(remotePath, "/")
	if root != "" {
		body["path"] = "/" + root
	}
	for {
		requestBody, _ := json.Marshal(body)
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(requestBody)))
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("Content-Type", "application/json")
		var result struct {
			Entries []struct {
				Tag         string `json:".tag"`
				ID          string `json:"id"`
				Name        string `json:"name"`
				PathDisplay string `json:"path_display"`
				Revision    string `json:"rev"`
				ContentHash string `json:"content_hash"`
				Size        int64  `json:"size"`
			} `json:"entries"`
			Cursor  string `json:"cursor"`
			HasMore bool   `json:"has_more"`
		}
		if err := cloudJSON(request, &result); err != nil {
			return err
		}
		for _, item := range result.Entries {
			if item.Tag != "file" || !audioExtensions[strings.ToLower(filepath.Ext(item.Name))] {
				continue
			}
			relative := strings.TrimPrefix(strings.TrimPrefix(item.PathDisplay, "/"), root+"/")
			if _, ok := safeCloudRelativePath(relative); !ok {
				continue
			}
			headers := map[string]string{"Dropbox-API-Arg": string(mustJSON(map[string]string{"path": item.PathDisplay}))}
			version := cloudVersion(item.ContentHash, item.Revision, item.Size)
			if err := syncer.add("dropbox:"+item.ID, version, relative, func(target string) error {
				return downloadCloudFile(ctx, "https://content.dropboxapi.com/2/files/download", accessToken, headers, target)
			}); err != nil {
				return err
			}
		}
		if !result.HasMore {
			return nil
		}
		endpoint = "https://api.dropboxapi.com/2/files/list_folder/continue"
		body = map[string]any{"cursor": result.Cursor}
	}
}

func syncOneDrive(ctx context.Context, accessToken, remotePath string, syncer *cloudSync) error {
	rootEndpoint := "https://graph.microsoft.com/v1.0/me/drive/root/children"
	if remotePath != "" {
		parts := splitCloudPath(remotePath)
		for index := range parts {
			parts[index] = url.PathEscape(parts[index])
		}
		rootEndpoint = "https://graph.microsoft.com/v1.0/me/drive/root:/" + strings.Join(parts, "/") + ":/children"
	}
	var walk func(string, string) error
	walk = func(endpoint, relative string) error {
		for endpoint != "" {
			var result struct {
				NextLink string `json:"@odata.nextLink"`
				Value    []struct {
					ID           string         `json:"id"`
					Name         string         `json:"name"`
					Folder       map[string]any `json:"folder"`
					DownloadURL  string         `json:"@microsoft.graph.downloadUrl"`
					ETag         string         `json:"eTag"`
					CTag         string         `json:"cTag"`
					ModifiedTime string         `json:"lastModifiedDateTime"`
					Size         int64          `json:"size"`
				} `json:"value"`
			}
			if err := cloudGET(ctx, endpoint, accessToken, &result); err != nil {
				return err
			}
			for _, item := range result.Value {
				name, ok := safeCloudName(item.Name)
				if !ok {
					continue
				}
				path := filepath.Join(relative, name)
				if item.Folder != nil {
					children := "https://graph.microsoft.com/v1.0/me/drive/items/" + url.PathEscape(item.ID) + "/children"
					if err := walk(children, path); err != nil {
						return err
					}
				} else if audioExtensions[strings.ToLower(filepath.Ext(name))] {
					downloadURL := item.DownloadURL
					token := ""
					if downloadURL == "" {
						downloadURL = "https://graph.microsoft.com/v1.0/me/drive/items/" + url.PathEscape(item.ID) + "/content"
						token = accessToken
					}
					version := cloudVersion(item.CTag, item.ETag+":"+item.ModifiedTime, item.Size)
					if err := syncer.add("onedrive:"+item.ID, version, path, func(target string) error {
						return downloadCloudFile(ctx, downloadURL, token, nil, target)
					}); err != nil {
						return err
					}
				}
			}
			endpoint = result.NextLink
		}
		return nil
	}
	return walk(rootEndpoint, "")
}

func cloudGET(ctx context.Context, endpoint, accessToken string, target any) error {
	return cloudGETWithScheme(ctx, endpoint, "Bearer", accessToken, target)
}

func cloudGETWithScheme(ctx context.Context, endpoint, scheme, accessToken string, target any) error {
	if !cloudURLAllowed(endpoint, accessToken != "") {
		return errors.New("cloud API URL rejected")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if accessToken != "" {
		request.Header.Set("Authorization", scheme+" "+accessToken)
	}
	return cloudJSON(request, target)
}

func cloudJSON(request *http.Request, target any) error {
	response, err := cloudHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("cloud API %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target)
}

func downloadCloudFile(ctx context.Context, endpoint, accessToken string, headers map[string]string, target string) error {
	if !cloudURLAllowed(endpoint, accessToken != "") {
		return errors.New("cloud download URL rejected")
	}
	method := http.MethodGet
	if strings.Contains(endpoint, "dropboxapi.com/2/files/download") {
		method = http.MethodPost
	}
	request, _ := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := cloudHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("cloud download %d", response.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, response.Body)
	closeErr := file.Close()
	return errors.Join(copyErr, closeErr)
}

func cloudURLAllowed(endpoint string, carriesToken bool) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	if !carriesToken {
		return true
	}
	allowed := map[string]bool{"www.googleapis.com": true, "cloud-api.yandex.net": true, "api.dropboxapi.com": true, "content.dropboxapi.com": true, "graph.microsoft.com": true}
	return allowed[strings.ToLower(parsed.Hostname())]
}

func sourceRequestValid(payload sourceRequest) bool {
	_, providerOK := cloudProviderSpec(payload.Provider)
	return providerOK && payload.Name != "" && len(payload.Name) <= 64 && !strings.ContainsAny(payload.Name, "/\\\r\n") && len(payload.RemotePath) <= 500 && !strings.ContainsAny(payload.RemotePath, ":\\\r\n") && !strings.Contains(payload.RemotePath, "..")
}

func cloudVersion(checksum, fallback string, size int64) string {
	if checksum == "" {
		checksum = fallback
	}
	return checksum + ":" + strconv.FormatInt(size, 10)
}

func oauthCredentialsValid(credentials oauthCredentials, allowEmpty bool) bool {
	if allowEmpty && credentials.ClientID == "" && credentials.ClientSecret == "" {
		return true
	}
	return credentials.ClientID != "" && credentials.ClientSecret != "" && len(credentials.ClientID) <= 512 && len(credentials.ClientSecret) <= 2048 && !strings.ContainsAny(credentials.ClientID+credentials.ClientSecret, "\x00\r\n")
}

func splitCloudPath(path string) []string {
	parts := []string{}
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func safeCloudName(name string) (string, bool) {
	return name, managedNameValid(name)
}

func safeCloudRelativePath(path string) (string, bool) {
	parts := splitCloudPath(path)
	for _, part := range parts {
		if !managedNameValid(part) {
			return "", false
		}
	}
	return strings.Join(parts, "/"), len(parts) > 0
}

func googleEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "\\'")
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func (application *app) sourceDir(id int64) string {
	return filepath.Join(application.dataDir, "cloud", strconv.FormatInt(id, 10))
}

func (application *app) encryptConfig(value []byte) ([]byte, error) {
	if len(application.secretKey) != 32 {
		return nil, errors.New("APP_SECRET must contain 32 bytes")
	}
	block, err := aes.NewCipher(application.secretKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, value, nil), nil
}

func (application *app) decryptConfig(value []byte) ([]byte, error) {
	if len(application.secretKey) != 32 {
		return nil, errors.New("APP_SECRET must contain 32 bytes")
	}
	block, err := aes.NewCipher(application.secretKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(value) < gcm.NonceSize() {
		return nil, errors.New("invalid encrypted config")
	}
	return gcm.Open(nil, value[:gcm.NonceSize()], value[gcm.NonceSize():], nil)
}
