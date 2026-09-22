package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func (application *app) updateStatus(response http.ResponseWriter, request *http.Request, _ user) {
	repository := env("UPDATE_REPOSITORY", "")
	result := map[string]any{"configured": false, "currentVersion": version}
	if !repositoryValid(repository) {
		writeJSON(response, http.StatusOK, result)
		return
	}
	apiRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось проверить обновления")
		return
	}
	apiRequest.Header.Set("Accept", "application/vnd.github+json")
	apiRequest.Header.Set("User-Agent", "Resonyr/"+version)
	apiResponse, err := updateClient.Do(apiRequest)
	if err != nil {
		writeError(response, http.StatusBadGateway, "GitHub недоступен")
		return
	}
	defer apiResponse.Body.Close()
	if apiResponse.StatusCode != http.StatusOK {
		writeError(response, http.StatusBadGateway, "Не удалось получить версию релиза")
		return
	}
	var release struct {
		TagName string `json:"tag_name"`
		URL     string `json:"html_url"`
	}
	if json.NewDecoder(io.LimitReader(apiResponse.Body, 1<<20)).Decode(&release) != nil {
		writeError(response, http.StatusBadGateway, "Некорректный ответ GitHub")
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
