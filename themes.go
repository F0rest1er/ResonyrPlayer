package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type themeManifest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Author  string `json:"author"`
	Preview string `json:"preview"`
}

func (application *app) themes(response http.ResponseWriter, request *http.Request, _ user) {
	root := filepath.Join(application.dataDir, "themes")
	entries, _ := os.ReadDir(root)
	items := []themeManifest{{ID: "", Name: "Системная", Version: "1"}}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "manifest.json"))
		var item themeManifest
		if err == nil && json.Unmarshal(data, &item) == nil {
			items = append(items, item)
		}
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) setTheme(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		Theme string `json:"theme"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if payload.Theme != "" && !themeIDValid(payload.Theme) {
		writeError(response, http.StatusBadRequest, "Некорректная тема")
		return
	}
	if payload.Theme != "" {
		if _, err := os.Stat(filepath.Join(application.dataDir, "themes", payload.Theme, "theme.css")); err != nil {
			writeError(response, http.StatusNotFound, "Тема не найдена")
			return
		}
	}
	_, err := application.db.Exec(request.Context(), "UPDATE users SET theme_id = $1 WHERE id = $2", payload.Theme, currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось сохранить тему")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) setColorMode(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		ColorMode string `json:"colorMode"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if payload.ColorMode != "system" && payload.ColorMode != "light" && payload.ColorMode != "dark" {
		writeError(response, http.StatusBadRequest, "Некорректная цветовая схема")
		return
	}
	_, err := application.db.Exec(request.Context(), "UPDATE users SET color_mode = $1 WHERE id = $2", payload.ColorMode, currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось сохранить цветовую схему")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) themeFile(response http.ResponseWriter, request *http.Request, _ user) {
	id := request.PathValue("id")
	file := request.PathValue("file")
	if !themeIDValid(id) || (file != "theme.css" && file != "preview.png" && !themeAssetValid(file)) {
		writeError(response, http.StatusBadRequest, "Некорректный файл темы")
		return
	}
	root := filepath.Join(application.dataDir, "themes", id)
	path, ok := safeMusicPath(root, file)
	if !ok {
		writeError(response, http.StatusBadRequest, "Некорректный путь")
		return
	}
	response.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(response, request, path)
}

func (application *app) uploadTheme(response http.ResponseWriter, request *http.Request, _ user) {
	request.Body = http.MaxBytesReader(response, request.Body, 12<<20)
	file, _, err := request.FormFile("theme")
	if err != nil {
		writeError(response, http.StatusBadRequest, "Загрузите ZIP-пакет темы")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(response, http.StatusBadRequest, "Не удалось прочитать тему")
		return
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) > 100 {
		writeError(response, http.StatusBadRequest, "Некорректный ZIP-пакет")
		return
	}
	root := filepath.Join(application.dataDir, "themes")
	if err := os.MkdirAll(root, 0o750); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось создать хранилище тем")
		return
	}
	temporary, err := os.MkdirTemp(root, "upload-")
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось обработать тему")
		return
	}
	defer os.RemoveAll(temporary)
	var total uint64
	for _, item := range archive.File {
		name := filepath.ToSlash(filepath.Clean(item.Name))
		if !themeArchiveEntryValid(name, item.FileInfo().IsDir()) || strings.Contains(item.Name, "\\") || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || item.Mode()&os.ModeSymlink != 0 {
			writeError(response, http.StatusBadRequest, "Пакет содержит недопустимые файлы")
			return
		}
		if item.FileInfo().IsDir() {
			continue
		}
		target := filepath.Join(temporary, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось распаковать тему")
			return
		}
		source, err := item.Open()
		if err != nil {
			writeError(response, http.StatusBadRequest, "Не удалось распаковать тему")
			return
		}
		targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
		if err == nil {
			var written int64
			written, err = io.Copy(targetFile, io.LimitReader(source, int64(20<<20-total)+1))
			total += uint64(written)
			if total > 20<<20 {
				err = errors.New("theme is too large")
			}
		}
		source.Close()
		if targetFile != nil {
			targetFile.Close()
		}
		if err != nil {
			writeError(response, http.StatusBadRequest, "Не удалось распаковать тему")
			return
		}
	}
	manifestData, err := os.ReadFile(filepath.Join(temporary, "manifest.json"))
	css, cssErr := os.ReadFile(filepath.Join(temporary, "theme.css"))
	var manifest themeManifest
	if err != nil || cssErr != nil || json.Unmarshal(manifestData, &manifest) != nil || !themeIDValid(manifest.ID) || strings.TrimSpace(manifest.Name) == "" {
		writeError(response, http.StatusBadRequest, "Требуются корректные manifest.json и theme.css")
		return
	}
	if !themeCSSValid(string(css)) {
		writeError(response, http.StatusBadRequest, "Внешние ресурсы в теме запрещены")
		return
	}
	if err := os.Rename(temporary, filepath.Join(root, manifest.ID)); err != nil {
		writeError(response, http.StatusConflict, "Тема с таким ID уже существует")
		return
	}
	writeJSON(response, http.StatusCreated, manifest)
}

func (application *app) deleteTheme(response http.ResponseWriter, request *http.Request, _ user) {
	id := request.PathValue("id")
	if !themeIDValid(id) {
		writeError(response, http.StatusBadRequest, "Некорректная тема")
		return
	}
	path := filepath.Join(application.dataDir, "themes", id)
	if _, err := os.Stat(path); err != nil {
		writeError(response, http.StatusNotFound, "Тема не найдена")
		return
	}
	if _, err := application.db.Exec(request.Context(), "UPDATE users SET theme_id = '' WHERE theme_id = $1", id); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось отключить тему")
		return
	}
	if err := os.RemoveAll(path); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось удалить тему")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func themeArchiveEntryValid(name string, isDirectory bool) bool {
	if isDirectory {
		return name == "assets" || strings.HasPrefix(name, "assets/") && !strings.Contains(name, "..")
	}
	return name == "manifest.json" || name == "theme.css" || name == "preview.png" || themeAssetValid(name)
}

func themeIDValid(value string) bool {
	if value == "" || len(value) > 50 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func themeAssetValid(value string) bool {
	if !strings.HasPrefix(value, "assets/") || strings.Contains(value, "..") {
		return false
	}
	switch strings.ToLower(filepath.Ext(value)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".woff", ".woff2", ".ttf", ".otf":
		return true
	}
	return false
}

func themeCSSValid(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "@import") || strings.Contains(lower, "javascript:") || strings.Contains(lower, "expression(") || strings.Contains(lower, "\\") {
		return false
	}
	for rest := lower; ; {
		start := strings.Index(rest, "url(")
		if start < 0 {
			return true
		}
		rest = rest[start+4:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return false
		}
		path := strings.Trim(strings.TrimSpace(rest[:end]), "\"'")
		if !themeAssetValid(path) {
			return false
		}
		rest = rest[end+1:]
	}
}
