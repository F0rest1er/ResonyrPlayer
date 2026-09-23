package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebUpdateGuards(t *testing.T) {
	application := &app{dataDir: t.TempDir()}
	for _, tag := range []string{"../main", "v1.2.3\nmain", "--upload-pack=bad", "$(id)"} {
		if updateTagPattern.MatchString(tag) {
			t.Fatalf("unsafe tag accepted: %q", tag)
		}
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/update", strings.NewReader(`{"version":"v1.2.3"}`))
	application.startWebUpdate(response, request, user{})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline worker: %d", response.Code)
	}

	root := filepath.Join(application.dataDir, "web-update")
	if err := os.MkdirAll(filepath.Join(root, "job"), 0o700); err != nil {
		t.Fatal(err)
	}
	heartbeat := filepath.Join(root, "heartbeat")
	if err := os.WriteFile(heartbeat, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if application.webUpdateState()["workerOnline"] != true {
		t.Fatal("heartbeat not detected")
	}
	previousClient := updateClient
	t.Cleanup(func() { updateClient = previousClient })
	t.Setenv("UPDATE_REPOSITORY", "F0rest1er/ResonyrPlayer")
	updateClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v1.2.3"}`))}, nil
	})}
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/admin/update", strings.NewReader(`{"version":"v1.2.3"}`))
	application.startWebUpdate(response, request, user{})
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate update: %d", response.Code)
	}
	stale := time.Now().Add(-time.Minute)
	if err := os.Chtimes(heartbeat, stale, stale); err != nil {
		t.Fatal(err)
	}
	if application.webUpdateState()["workerOnline"] != false {
		t.Fatal("stale heartbeat accepted")
	}
}
