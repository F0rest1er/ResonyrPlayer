package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactPlaylistCover(t *testing.T) {
	var source bytes.Buffer
	if err := png.Encode(&source, image.NewRGBA(image.Rect(0, 0, 2048, 2048))); err != nil {
		t.Fatal(err)
	}
	result, err := compactPlaylistCover(source.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(result))
	if err != nil || config.Width != 640 || config.Height != 640 || format != "jpeg" {
		t.Fatalf("unexpected thumbnail: %+v %s %v", config, format, err)
	}
}

func TestScanSkipsUnchangedWithoutDatabaseQuery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "track.mp3")
	if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	indexed := map[string]indexedTrack{"track.mp3": {size: info.Size(), modified: info.ModTime().Truncate(time.Microsecond)}}
	seen := []string{}
	if err := (&app{}).scanRoot(context.Background(), root, "", &seen, indexed); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatal(seen)
	}
}

func TestBackupDeleteAndRestoreGuards(t *testing.T) {
	application := &app{dataDir: t.TempDir()}
	root := filepath.Join(application.dataDir, "backups")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "player-20260925-120000.123456789.dump"
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"confirm":"wrong"}`))
	request.SetPathValue("name", name)
	response := httptest.NewRecorder()
	application.restoreBackup(response, request, user{})
	if response.Code != http.StatusBadRequest {
		t.Fatal(response.Code)
	}
	request = httptest.NewRequest(http.MethodDelete, "/", nil)
	request.SetPathValue("name", name)
	response = httptest.NewRecorder()
	application.deleteBackup(response, request, user{})
	if response.Code != http.StatusNoContent {
		t.Fatal(response.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("backup was not deleted")
	}
	request.SetPathValue("name", "../outside.dump")
	response = httptest.NewRecorder()
	application.deleteBackup(response, request, user{})
	if response.Code != http.StatusBadRequest {
		t.Fatal(response.Code)
	}
}
