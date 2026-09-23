package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	URL     string `json:"html_url"`
}

func latestReleaseVersion(ctx context.Context) (githubRelease, error) {
	repository := env("UPDATE_REPOSITORY", "")
	if !repositoryValid(repository) {
		return githubRelease{}, fmt.Errorf("Укажите UPDATE_REPOSITORY в .env")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return githubRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Resonyr/"+version)
	response, err := updateClient.Do(request)
	if err != nil {
		return githubRelease{}, fmt.Errorf("GitHub недоступен")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		var release githubRelease
		if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release) == nil && release.TagName != "" {
			return release, nil
		}
	}
	if response.StatusCode != http.StatusNotFound {
		return githubRelease{}, fmt.Errorf("Не удалось получить релиз GitHub")
	}
	tagsRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/tags", nil)
	if err != nil {
		return githubRelease{}, err
	}
	tagsRequest.Header.Set("Accept", "application/vnd.github+json")
	tagsRequest.Header.Set("User-Agent", "Resonyr/"+version)
	tagsResponse, err := updateClient.Do(tagsRequest)
	if err != nil {
		return githubRelease{}, fmt.Errorf("GitHub недоступен")
	}
	defer tagsResponse.Body.Close()
	var tags []struct {
		Name string `json:"name"`
	}
	if tagsResponse.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(tagsResponse.Body, 1<<20)).Decode(&tags) == nil && len(tags) > 0 {
		return githubRelease{
			TagName: tags[0].Name,
			URL:     "https://github.com/" + repository + "/releases/tag/" + tags[0].Name,
		}, nil
	}
	return githubRelease{}, fmt.Errorf("В репозитории пока нет опубликованных релизов или тегов")
}

var updateTagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$`)

func (application *app) webUpdateState() map[string]any {
	root := filepath.Join(application.dataDir, "web-update")
	heartbeat, err := os.Stat(filepath.Join(root, "heartbeat"))
	online := err == nil && time.Since(heartbeat.ModTime()) < 30*time.Second
	status, _ := os.ReadFile(filepath.Join(root, "status"))
	lastErr, _ := os.ReadFile(filepath.Join(root, "error"))
	_, jobErr := os.Stat(filepath.Join(root, "job"))
	return map[string]any{"workerOnline": online, "busy": jobErr == nil, "status": strings.TrimSpace(string(status)), "error": strings.TrimSpace(string(lastErr)), "currentVersion": version}
}

func (application *app) webUpdateStatus(response http.ResponseWriter, _ *http.Request, _ user) {
	writeJSON(response, http.StatusOK, application.webUpdateState())
}

func (application *app) startWebUpdate(response http.ResponseWriter, request *http.Request, _ user) {
	var payload struct {
		Version string `json:"version"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if len(payload.Version) > 80 || !updateTagPattern.MatchString(payload.Version) {
		writeError(response, http.StatusBadRequest, "Релиз должен иметь тег вида v1.2.3")
		return
	}
	if application.webUpdateState()["workerOnline"] != true {
		writeError(response, http.StatusServiceUnavailable, "На хосте не запущен web-updater.sh")
		return
	}
	release, err := latestReleaseVersion(request.Context())
	if err != nil {
		writeError(response, http.StatusBadGateway, err.Error())
		return
	}
	if release.TagName != payload.Version || release.TagName == version {
		writeError(response, http.StatusConflict, "Список версий изменился. Обновите страницу")
		return
	}
	root := filepath.Join(application.dataDir, "web-update")
	job := filepath.Join(root, "job")
	if os.Mkdir(job, 0o700) != nil {
		writeError(response, http.StatusConflict, "Обновление уже запрошено")
		return
	}
	_ = os.WriteFile(filepath.Join(root, "status"), []byte("backup"), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err = application.createBackupFile(ctx); err == nil {
		err = os.WriteFile(filepath.Join(job, "pending"), []byte(release.TagName), 0o600)
		if err == nil {
			err = os.Rename(filepath.Join(job, "pending"), filepath.Join(job, "ready"))
		}
	}
	if err != nil {
		_ = os.Remove(filepath.Join(job, "ready"))
		_ = os.Remove(filepath.Join(job, "pending"))
		_ = os.Remove(job)
		_ = os.WriteFile(filepath.Join(root, "status"), []byte("failed"), 0o600)
		writeError(response, http.StatusInternalServerError, "Не удалось подготовить обновление и резервную копию")
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (application *app) updateStatus(response http.ResponseWriter, request *http.Request, _ user) {
	repository := env("UPDATE_REPOSITORY", "")
	result := map[string]any{"configured": false, "currentVersion": version}
	if !repositoryValid(repository) {
		writeJSON(response, http.StatusOK, result)
		return
	}
	release, err := latestReleaseVersion(request.Context())
	if err != nil {
		writeError(response, http.StatusBadGateway, err.Error())
		return
	}
	result["configured"] = true
	result["latestVersion"] = release.TagName
	result["updateAvailable"] = release.TagName != "" && release.TagName != version
	result["url"] = release.URL
	writeJSON(response, http.StatusOK, result)
}

func repositoryValid(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || len(value) > 150 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' && char != '/' {
			return false
		}
	}
	return true
}

var updateClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}
