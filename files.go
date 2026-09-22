package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type managedFile struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	IsDir      bool   `json:"isDir"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modifiedAt"`
}

func (application *app) files(response http.ResponseWriter, request *http.Request, _ user) {
	relativePath := cleanLibraryPath(request.URL.Query().Get("path"))
	directory, ok := managedPath(application.musicDir, relativePath, true)
	if !ok {
		writeError(response, http.StatusBadRequest, "Некорректный путь")
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		writeError(response, http.StatusNotFound, "Папка не найдена")
		return
	}
	items := make([]managedFile, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		items = append(items, managedFile{Name: entry.Name(), Path: cleanLibraryPath(filepath.ToSlash(filepath.Join(relativePath, entry.Name()))), IsDir: entry.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().Format(time.RFC3339)})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].IsDir != items[right].IsDir {
			return items[left].IsDir
		}
		return strings.ToLower(items[left].Name) < strings.ToLower(items[right].Name)
	})
	writeJSON(response, http.StatusOK, map[string]any{"path": relativePath, "items": items})
}

func (application *app) createFolder(response http.ResponseWriter, request *http.Request, _ user) {
	var payload struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if !managedNameValid(payload.Name) {
		writeError(response, http.StatusBadRequest, "Некорректное название папки")
		return
	}
	target, ok := managedPath(application.musicDir, filepath.ToSlash(filepath.Join(cleanLibraryPath(payload.Path), payload.Name)), false)
	if !ok {
		writeError(response, http.StatusBadRequest, "Некорректный путь")
		return
	}
	if err := os.Mkdir(target, 0o750); err != nil {
		writeError(response, http.StatusConflict, "Не удалось создать папку")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) renameFile(response http.ResponseWriter, request *http.Request, _ user) {
	var payload struct {
		Name            string  `json:"name"`
		DestinationPath *string `json:"destinationPath"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if !managedNameValid(payload.Name) {
		writeError(response, http.StatusBadRequest, "Некорректное название")
		return
	}
	oldRelative := cleanLibraryPath(request.URL.Query().Get("path"))
	if oldRelative == "" {
		writeError(response, http.StatusBadRequest, "Корневую папку нельзя переименовать")
		return
	}
	newRelative, targetOK := managedMoveTarget(oldRelative, payload.Name, payload.DestinationPath)
	if !targetOK {
		writeError(response, http.StatusBadRequest, "Нельзя переместить папку внутрь самой себя")
		return
	}
	if newRelative == oldRelative {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	oldPath, ok := managedPath(application.musicDir, oldRelative, true)
	newPath, newOK := managedPath(application.musicDir, newRelative, false)
	if !ok || !newOK {
		writeError(response, http.StatusBadRequest, "Некорректный путь")
		return
	}
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		writeError(response, http.StatusConflict, "Файл или папка с таким названием уже существует")
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось переименовать")
		return
	}
	oldLibraryPath := filepath.ToSlash(oldRelative)
	newLibraryPath := filepath.ToSlash(newRelative)
	rows, err := application.db.Query(request.Context(), "SELECT id, path FROM tracks WHERE path = $1 OR LEFT(path, LENGTH($1) + 1) = $1 || '/'", oldLibraryPath)
	if err == nil {
		updates := map[int64]string{}
		for rows.Next() {
			var id int64
			var path string
			if rows.Scan(&id, &path) == nil {
				updates[id] = newLibraryPath + strings.TrimPrefix(path, oldLibraryPath)
			}
		}
		rows.Close()
		for id, path := range updates {
			_, _ = application.db.Exec(request.Context(), "UPDATE tracks SET path = $2 WHERE id = $1", id, path)
		}
	}
	response.WriteHeader(http.StatusNoContent)
}

func managedMoveTarget(oldRelative, name string, destinationPath *string) (string, bool) {
	destination := cleanLibraryPath(filepath.ToSlash(filepath.Dir(oldRelative)))
	if destinationPath != nil {
		for _, part := range strings.Split(filepath.ToSlash(*destinationPath), "/") {
			if part == ".." {
				return "", false
			}
		}
		destination = cleanLibraryPath(*destinationPath)
	}
	if destination == oldRelative || strings.HasPrefix(destination, oldRelative+"/") {
		return "", false
	}
	return cleanLibraryPath(filepath.ToSlash(filepath.Join(destination, name))), true
}

func (application *app) deleteFile(response http.ResponseWriter, request *http.Request, _ user) {
	relativePath := cleanLibraryPath(request.URL.Query().Get("path"))
	if relativePath == "" {
		writeError(response, http.StatusBadRequest, "Корневую папку нельзя удалить")
		return
	}
	target, ok := managedPath(application.musicDir, relativePath, true)
	if !ok {
		writeError(response, http.StatusBadRequest, "Некорректный путь")
		return
	}
	if err := os.RemoveAll(target); err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось удалить")
		return
	}
	if err := application.scan(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Файл удалён, но медиатека не обновлена")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) uploadFiles(response http.ResponseWriter, request *http.Request, currentUser user) {
	relativePath := cleanLibraryPath(request.URL.Query().Get("path"))
	uploadDir, ok := managedPath(application.musicDir, relativePath, true)
	if !ok {
		writeError(response, http.StatusBadRequest, "Некорректная папка загрузки")
		return
	}
	application.saveUploads(response, request, currentUser, uploadDir)
}

func managedPath(root, relativePath string, existing bool) (string, bool) {
	if strings.ContainsAny(relativePath, "\x00\\") || strings.HasPrefix(relativePath, "/") {
		return "", false
	}
	for _, part := range strings.Split(filepath.ToSlash(relativePath), "/") {
		if part == ".." {
			return "", false
		}
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	if err := os.MkdirAll(absoluteRoot, 0o750); err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", false
	}
	target := filepath.Join(absoluteRoot, filepath.FromSlash(relativePath))
	checked := target
	if !existing {
		checked = filepath.Dir(target)
	}
	realChecked, err := filepath.EvalSymlinks(checked)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(realRoot, realChecked)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if existing {
		return realChecked, true
	}
	return target, true
}

func managedNameValid(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 240 && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func (application *app) saveUploads(response http.ResponseWriter, request *http.Request, currentUser user, uploadDir string) {
	request.Body = http.MaxBytesReader(response, request.Body, 100<<30)
	reader, err := request.MultipartReader()
	if err != nil {
		writeError(response, http.StatusBadRequest, "Некорректная загрузка")
		return
	}
	uploaded := 0
	fileIndex := 0
	relativePaths := []string{}
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			writeError(response, http.StatusBadRequest, "Не удалось прочитать файлы")
			return
		}
		if part.FormName() == "relativePaths" && part.FileName() == "" {
			data, readErr := io.ReadAll(io.LimitReader(part, 1<<20+1))
			part.Close()
			if readErr != nil || len(data) > 1<<20 || json.Unmarshal(data, &relativePaths) != nil {
				writeError(response, http.StatusBadRequest, "Некорректная структура папки")
				return
			}
			continue
		}
		name := safeUploadName(part.FileName())
		if part.FormName() != "files" || name == "" || !audioExtensions[strings.ToLower(filepath.Ext(name))] {
			part.Close()
			writeError(response, http.StatusBadRequest, "Разрешены AAC, FLAC, M4A, MP3, OGG, OPUS и WAV")
			return
		}
		fileDir := uploadDir
		if fileIndex < len(relativePaths) && relativePaths[fileIndex] != "" {
			relativePath, ok := safeUploadRelativePath(relativePaths[fileIndex])
			if !ok || filepath.Base(filepath.FromSlash(relativePath)) != name {
				part.Close()
				writeError(response, http.StatusBadRequest, "Некорректный путь файла")
				return
			}
			var directoryOK bool
			fileDir, directoryOK = ensureUploadDirectory(uploadDir, filepath.Dir(filepath.FromSlash(relativePath)))
			if !directoryOK {
				part.Close()
				writeError(response, http.StatusBadRequest, "Не удалось создать структуру папки")
				return
			}
		}
		fileIndex++
		temporary, createErr := os.CreateTemp(fileDir, ".upload-*")
		if createErr != nil {
			part.Close()
			writeError(response, http.StatusInternalServerError, "Не удалось сохранить файл")
			return
		}
		written, copyErr := io.Copy(temporary, io.LimitReader(part, 1<<30+1))
		part.Close()
		closeErr := temporary.Close()
		if copyErr != nil || closeErr != nil || written > 1<<30 {
			_ = os.Remove(temporary.Name())
			writeError(response, http.StatusBadRequest, "Один файл не должен превышать 1 ГБ")
			return
		}
		target := availableUploadPath(fileDir, name)
		if err := os.Rename(temporary.Name(), target); err != nil {
			_ = os.Remove(temporary.Name())
			writeError(response, http.StatusInternalServerError, "Не удалось завершить загрузку")
			return
		}
		_ = os.Chmod(target, 0o640)
		uploaded++
	}
	if uploaded == 0 {
		writeError(response, http.StatusBadRequest, "Выберите аудиофайлы")
		return
	}
	if err := application.scan(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, "Файлы загружены, но сканирование завершилось с ошибкой")
		return
	}
	application.audit(request.Context(), currentUser.ID, "Загружено музыкальных файлов: "+strconv.Itoa(uploaded))
	writeJSON(response, http.StatusCreated, map[string]int{"uploaded": uploaded})
}

func safeUploadRelativePath(value string) (string, bool) {
	value = filepath.ToSlash(value)
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", false
	}
	for _, part := range strings.Split(value, "/") {
		if !managedNameValid(part) {
			return "", false
		}
	}
	return value, true
}

func ensureUploadDirectory(root, relativePath string) (string, bool) {
	directory := root
	if relativePath == "." || relativePath == "" {
		return directory, true
	}
	for _, part := range strings.Split(filepath.ToSlash(relativePath), "/") {
		if !managedNameValid(part) {
			return "", false
		}
		directory = filepath.Join(directory, part)
		if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
			return "", false
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return directory, true
}
