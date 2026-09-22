package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type auditEntry struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Event     string `json:"event"`
	CreatedAt string `json:"createdAt"`
}

type backupFile struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
}

func (application *app) audit(ctx context.Context, userID int64, event string) {
	if len(event) > 500 {
		event = event[:500]
	}
	_, _ = application.db.Exec(ctx, "INSERT INTO audit_logs (user_id, event) VALUES ($1, $2)", userID, event)
}

func (application *app) auditLogs(response http.ResponseWriter, request *http.Request, _ user) {
	rows, err := application.db.Query(request.Context(), `SELECT l.id, COALESCE(u.username, 'system'), l.event, l.created_at
		FROM audit_logs l LEFT JOIN users u ON u.id = l.user_id ORDER BY l.created_at DESC LIMIT 200`)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить журнал")
		return
	}
	defer rows.Close()
	items := []auditEntry{}
	for rows.Next() {
		var item auditEntry
		var createdAt time.Time
		if rows.Scan(&item.ID, &item.Username, &item.Event, &createdAt) != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать журнал")
			return
		}
		item.CreatedAt = createdAt.Format(time.RFC3339)
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) backups(response http.ResponseWriter, _ *http.Request, _ user) {
	writeJSON(response, http.StatusOK, application.backupFiles())
}

func (application *app) createBackup(response http.ResponseWriter, request *http.Request, _ user) {
	file, err := application.createBackupFile(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось создать резервную копию")
		return
	}
	writeJSON(response, http.StatusCreated, file)
}

func (application *app) downloadBackup(response http.ResponseWriter, request *http.Request, _ user) {
	name := request.PathValue("name")
	if !backupNameValid(name) {
		writeError(response, http.StatusBadRequest, "Некорректное имя копии")
		return
	}
	path := filepath.Join(application.dataDir, "backups", name)
	if _, err := os.Stat(path); err != nil {
		writeError(response, http.StatusNotFound, "Резервная копия не найдена")
		return
	}
	response.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	response.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(response, request, path)
}

func (application *app) createBackupFile(ctx context.Context) (backupFile, error) {
	root := filepath.Join(application.dataDir, "backups")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return backupFile{}, err
	}
	name := "player-" + time.Now().UTC().Format("20060102-150405") + ".dump"
	path := filepath.Join(root, name)
	command := exec.CommandContext(ctx, "pg_dump", "--dbname", env("DATABASE_URL", ""), "--format", "custom", "--file", path)
	if output, err := command.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return backupFile{}, fmt.Errorf("pg_dump: %s", strings.TrimSpace(string(output)))
	}
	_ = os.Chmod(path, 0o600)
	application.pruneBackups(10)
	info, err := os.Stat(path)
	if err != nil {
		return backupFile{}, err
	}
	return backupFile{Name: name, Size: info.Size(), CreatedAt: info.ModTime().Format(time.RFC3339)}, nil
}

func (application *app) backupFiles() []backupFile {
	root := filepath.Join(application.dataDir, "backups")
	entries, _ := os.ReadDir(root)
	items := []backupFile{}
	for _, entry := range entries {
		if entry.IsDir() || !backupNameValid(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			items = append(items, backupFile{Name: entry.Name(), Size: info.Size(), CreatedAt: info.ModTime().Format(time.RFC3339)})
		}
	}
	sort.Slice(items, func(left, right int) bool { return items[left].Name > items[right].Name })
	return items
}

func (application *app) pruneBackups(keep int) {
	files := application.backupFiles()
	if len(files) <= keep {
		return
	}
	for _, file := range files[keep:] {
		_ = os.Remove(filepath.Join(application.dataDir, "backups", file.Name))
	}
}

func (application *app) backupLoop(ctx context.Context) {
	interval, err := time.ParseDuration(env("BACKUP_INTERVAL", "24h"))
	if err != nil || interval < time.Hour {
		interval = 24 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := application.createBackupFile(ctx); err != nil {
				application.notifyAll(ctx, "Ошибка резервного копирования", "Проверьте журнал сервера")
			}
		}
	}
}

func backupNameValid(value string) bool {
	if !strings.HasPrefix(value, "player-") || !strings.HasSuffix(value, ".dump") || len(value) != len("player-20060102-150405.dump") {
		return false
	}
	for _, char := range strings.TrimSuffix(strings.TrimPrefix(value, "player-"), ".dump") {
		if (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}
