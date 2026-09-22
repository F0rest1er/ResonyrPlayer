package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhowden/tag"
	webpush "github.com/ergochat/webpush-go/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

//go:embed web/*
var webFiles embed.FS

type app struct {
	db            *pgxpool.Pool
	dataDir       string
	devices       deviceHub
	loginAttempts map[string]loginAttempt
	loginLock     sync.Mutex
	musicDir      string
	scanLock      sync.Mutex
	secretKey     []byte
	uploadsDir    string
	vapidKeys     *webpush.VAPIDKeys
}

type loginAttempt struct {
	count int
	since time.Time
}

type user struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	IsAdmin   bool   `json:"isAdmin"`
	Theme     string `json:"theme"`
	ColorMode string `json:"colorMode"`
	Token     string `json:"token,omitempty"`
}

type track struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	Artists     []string `json:"artists"`
	Album       string   `json:"album"`
	AlbumArtist string   `json:"albumArtist"`
	Year        int      `json:"year"`
	TrackNumber int      `json:"trackNumber"`
	DiscNumber  int      `json:"discNumber"`
	Genre       string   `json:"genre"`
	Composer    string   `json:"composer"`
	Comment     string   `json:"comment"`
	Duration    float64  `json:"duration"`
	Bitrate     int      `json:"bitrate"`
	SampleRate  int      `json:"sampleRate"`
	Path        string   `json:"path"`
	Format      string   `json:"format"`
	HasCover    bool     `json:"hasCover"`
	Favorite    bool     `json:"favorite"`
}

type trackMetadata struct {
	Title       string
	Artist      string
	Artists     []string
	Album       string
	AlbumArtist string
	Year        int
	TrackNumber int
	DiscNumber  int
	Genre       string
	Composer    string
	Comment     string
	Duration    float64
	Bitrate     int
	SampleRate  int
	Format      string
	Raw         []byte
	Cover       []byte
	CoverMIME   string
}

type folder struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type libraryResponse struct {
	Path    string   `json:"path"`
	Folders []folder `json:"folders"`
	Tracks  []track  `json:"tracks"`
}

var audioExtensions = map[string]bool{
	".aac": true, ".flac": true, ".m4a": true, ".mp3": true, ".ogg": true, ".opus": true, ".wav": true,
}

const trackSelectColumns = `t.id, t.title, t.artist, t.artists, t.album, t.album_artist, t.year,
	t.track_number, t.disc_number, t.genre, t.composer, t.comment, t.duration, t.bitrate,
	t.sample_rate, t.path, t.format, (COALESCE(OCTET_LENGTH(t.cover), 0) > 0), (f.track_id IS NOT NULL)`

var version = "dev"

func main() {
	ctx := context.Background()
	databaseURL := env("DATABASE_URL", "postgres://player:player@localhost:5432/player?sslmode=disable")
	db, err := connectDB(ctx, databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	secretKey, _ := hex.DecodeString(env("APP_SECRET", ""))
	application := &app{db: db, musicDir: env("MUSIC_DIR", "./music"), uploadsDir: env("UPLOADS_DIR", "./uploads"), dataDir: env("DATA_DIR", "./data"), loginAttempts: map[string]loginAttempt{}, secretKey: secretKey}
	if err := application.migrate(ctx); err != nil {
		log.Fatal(err)
	}
	if err := application.cleanupLegacySources(ctx); err != nil {
		log.Fatal(err)
	}
	if err := application.loadPushKeys(ctx); err != nil {
		log.Fatal(err)
	}
	if err := application.ensureAdmin(ctx); err != nil {
		log.Fatal(err)
	}
	if err := application.syncSources(ctx); err != nil {
		log.Printf("первичная синхронизация источников: %v", err)
	}
	if err := application.scan(ctx); err != nil {
		log.Printf("первичное сканирование: %v", err)
	}
	go application.scanLoop(ctx)
	go application.releaseLoop(ctx)
	go application.backupLoop(ctx)

	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", application.login)
	mux.HandleFunc("POST /api/logout", application.auth(application.logout))
	mux.HandleFunc("GET /api/me", application.auth(application.me))
	mux.HandleFunc("PUT /api/account", application.auth(application.updateAccount))
	mux.HandleFunc("GET /api/themes", application.auth(application.themes))
	mux.HandleFunc("GET /api/themes/{id}/{file...}", application.auth(application.themeFile))
	mux.HandleFunc("PUT /api/settings/theme", application.auth(application.setTheme))
	mux.HandleFunc("PUT /api/settings/color-mode", application.auth(application.setColorMode))
	mux.HandleFunc("GET /api/library", application.auth(application.library))
	mux.HandleFunc("GET /api/search", application.auth(application.search))
	mux.HandleFunc("GET /api/tracks", application.auth(application.tracksByIDs))
	mux.HandleFunc("GET /api/tracks/{id}/stream", application.auth(application.stream))
	mux.HandleFunc("GET /api/tracks/{id}/cover", application.auth(application.cover))
	mux.HandleFunc("PUT /api/tracks/{id}/favorite", application.auth(application.favorite))
	mux.HandleFunc("PUT /api/admin/tracks/{id}", application.authAdmin(application.updateTrackMetadata))
	mux.HandleFunc("GET /api/playlists", application.auth(application.playlists))
	mux.HandleFunc("POST /api/playlists", application.auth(application.createPlaylist))
	mux.HandleFunc("GET /api/playlists/{id}", application.auth(application.playlistTracks))
	mux.HandleFunc("PUT /api/playlists/{id}", application.auth(application.updatePlaylist))
	mux.HandleFunc("DELETE /api/playlists/{id}", application.auth(application.deletePlaylist))
	mux.HandleFunc("GET /api/playlists/{id}/cover", application.auth(application.playlistCover))
	mux.HandleFunc("PUT /api/playlists/{id}/cover", application.auth(application.updatePlaylistCover))
	mux.HandleFunc("POST /api/playlists/{id}/tracks", application.auth(application.addPlaylistTrack))
	mux.HandleFunc("DELETE /api/playlists/{id}/tracks/{trackId}", application.auth(application.removePlaylistTrack))
	mux.HandleFunc("GET /api/history", application.auth(application.history))
	mux.HandleFunc("POST /api/history", application.auth(application.addHistory))
	mux.HandleFunc("GET /api/playback", application.auth(application.playbackState))
	mux.HandleFunc("PUT /api/playback", application.auth(application.savePlaybackState))
	mux.HandleFunc("POST /api/playback/control", application.auth(application.playbackControl))
	mux.HandleFunc("GET /api/devices", application.auth(application.listDevices))
	mux.HandleFunc("POST /api/devices/register", application.auth(application.registerDevice))
	mux.HandleFunc("PUT /api/devices/active", application.auth(application.activateDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", application.auth(application.deleteDevice))
	mux.HandleFunc("GET /api/devices/events", application.auth(application.deviceEvents))
	mux.HandleFunc("GET /api/notifications", application.auth(application.notifications))
	mux.HandleFunc("DELETE /api/notifications", application.auth(application.clearNotifications))
	mux.HandleFunc("GET /api/push/key", application.auth(application.pushKey))
	mux.HandleFunc("PUT /api/push/subscription", application.auth(application.savePushSubscription))
	mux.HandleFunc("DELETE /api/push/subscription", application.auth(application.deletePushSubscription))
	mux.HandleFunc("GET /api/watched-artists", application.auth(application.watchedArtists))
	mux.HandleFunc("POST /api/watched-artists", application.auth(application.addWatchedArtist))
	mux.HandleFunc("DELETE /api/watched-artists/{id}", application.auth(application.deleteWatchedArtist))
	mux.HandleFunc("GET /api/artists/search", application.auth(application.searchArtists))
	mux.HandleFunc("GET /api/artists/image", application.auth(application.artistImage))
	mux.HandleFunc("GET /api/admin/users", application.authAdmin(application.adminUsers))
	mux.HandleFunc("POST /api/admin/users", application.authAdmin(application.createUser))
	mux.HandleFunc("PUT /api/admin/users/{id}", application.authAdmin(application.updateUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", application.authAdmin(application.deleteUser))
	mux.HandleFunc("POST /api/admin/themes", application.authAdmin(application.uploadTheme))
	mux.HandleFunc("DELETE /api/admin/themes/{id}", application.authAdmin(application.deleteTheme))
	mux.HandleFunc("POST /api/admin/scan", application.authAdmin(application.rescan))
	mux.HandleFunc("GET /api/admin/sources", application.authAdmin(application.sources))
	mux.HandleFunc("POST /api/admin/sources/oauth/start", application.authAdmin(application.startSourceOAuth))
	mux.HandleFunc("GET /api/admin/sources/oauth/callback", application.authAdmin(application.sourceOAuthCallback))
	mux.HandleFunc("DELETE /api/admin/sources/{id}", application.authAdmin(application.deleteSource))
	mux.HandleFunc("POST /api/admin/sources/{id}/sync", application.authAdmin(application.syncSourceHandler))
	mux.HandleFunc("GET /api/admin/logs", application.authAdmin(application.auditLogs))
	mux.HandleFunc("GET /api/admin/backups", application.authAdmin(application.backups))
	mux.HandleFunc("POST /api/admin/backups", application.authAdmin(application.createBackup))
	mux.HandleFunc("GET /api/admin/backups/{name}", application.authAdmin(application.downloadBackup))
	mux.HandleFunc("GET /api/admin/update", application.authAdmin(application.updateStatus))
	mux.HandleFunc("POST /api/admin/music", application.authAdmin(application.uploadMusic))
	mux.HandleFunc("GET /api/admin/files", application.authAdmin(application.files))
	mux.HandleFunc("POST /api/admin/files/folders", application.authAdmin(application.createFolder))
	mux.HandleFunc("PUT /api/admin/files", application.authAdmin(application.renameFile))
	mux.HandleFunc("DELETE /api/admin/files", application.authAdmin(application.deleteFile))
	mux.HandleFunc("POST /api/admin/files/upload", application.authAdmin(application.uploadFiles))
	mux.HandleFunc("GET /health", func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) })
	mux.Handle("/", http.FileServer(http.FS(assets)))

	server := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           securityHeaders(csrfProtection(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("Resonyr запущен на %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func connectDB(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		db, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			err = db.Ping(ctx)
		}
		if err == nil {
			return db, nil
		}
		if db != nil {
			db.Close()
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("подключение к PostgreSQL: %w", lastErr)
}

func (application *app) migrate(ctx context.Context) error {
	_, err := application.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS tracks (
			id BIGSERIAL PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			title TEXT NOT NULL,
			artist TEXT NOT NULL DEFAULT '',
			album TEXT NOT NULL DEFAULT '',
			format TEXT NOT NULL,
			size BIGINT NOT NULL,
			modified_at TIMESTAMPTZ NOT NULL,
			indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS favorites (
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			track_id BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
			PRIMARY KEY (user_id, track_id)
		);
		CREATE INDEX IF NOT EXISTS tracks_title_idx ON tracks (LOWER(title));
		CREATE INDEX IF NOT EXISTS tracks_artist_idx ON tracks (LOWER(artist));
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS artists TEXT[] NOT NULL DEFAULT '{}';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS album_artist TEXT NOT NULL DEFAULT '';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS year INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS track_number INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS disc_number INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS genre TEXT NOT NULL DEFAULT '';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS composer TEXT NOT NULL DEFAULT '';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS comment TEXT NOT NULL DEFAULT '';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS duration DOUBLE PRECISION NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS bitrate INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS sample_rate INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS raw_metadata JSONB NOT NULL DEFAULT '{}';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS cover BYTEA;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS cover_mime TEXT NOT NULL DEFAULT '';
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS metadata_version INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE tracks ADD COLUMN IF NOT EXISTS metadata_edited BOOLEAN NOT NULL DEFAULT FALSE;
		ALTER TABLE users ADD COLUMN IF NOT EXISTS theme_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS color_mode TEXT NOT NULL DEFAULT 'system';
		CREATE TABLE IF NOT EXISTS playlists (
			id BIGSERIAL PRIMARY KEY,
			owner_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE playlists ADD COLUMN IF NOT EXISTS cover BYTEA;
		ALTER TABLE playlists ADD COLUMN IF NOT EXISTS cover_mime TEXT NOT NULL DEFAULT '';
		ALTER TABLE playlists ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
		CREATE TABLE IF NOT EXISTS playlist_shares (
			playlist_id BIGINT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			permission TEXT NOT NULL CHECK (permission IN ('view', 'add', 'edit')),
			PRIMARY KEY (playlist_id, user_id)
		);
		CREATE TABLE IF NOT EXISTS playlist_tracks (
			playlist_id BIGINT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
			track_id BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
			position INTEGER NOT NULL,
			added_by BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			PRIMARY KEY (playlist_id, track_id)
		);
		CREATE TABLE IF NOT EXISTS listening_history (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			track_id BIGINT NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
			position DOUBLE PRECISION NOT NULL DEFAULT 0,
			listened_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS listening_history_user_idx ON listening_history (user_id, listened_at DESC);
		CREATE TABLE IF NOT EXISTS playback_states (
			user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			current_track_id BIGINT REFERENCES tracks(id) ON DELETE SET NULL,
			position DOUBLE PRECISION NOT NULL DEFAULT 0,
			queue JSONB NOT NULL DEFAULT '[]',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		ALTER TABLE playback_states ADD COLUMN IF NOT EXISTS is_playing BOOLEAN NOT NULL DEFAULT FALSE;
		CREATE TABLE IF NOT EXISTS devices (
			id TEXT NOT NULL,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			is_active BOOLEAN NOT NULL DEFAULT FALSE,
			last_seen TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (user_id, id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS devices_one_active_idx ON devices (user_id) WHERE is_active;
		CREATE TABLE IF NOT EXISTS server_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS notifications (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS notifications_user_idx ON notifications (user_id, created_at DESC);
		CREATE TABLE IF NOT EXISTS push_subscriptions (
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			endpoint TEXT NOT NULL,
			subscription JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (user_id, endpoint)
		);
		CREATE TABLE IF NOT EXISTS music_sources (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			provider TEXT NOT NULL,
			remote_name TEXT NOT NULL,
			remote_path TEXT NOT NULL DEFAULT '',
			config BYTEA NOT NULL,
			last_sync_at TIMESTAMPTZ,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS oauth_states (
			state_hash TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			name TEXT NOT NULL,
			remote_path TEXT NOT NULL DEFAULT '',
			expires_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS oauth_credentials (
			provider TEXT PRIMARY KEY,
			config BYTEA NOT NULL
		);
		CREATE TABLE IF NOT EXISTS watched_artists (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			mbid TEXT NOT NULL,
			last_release_id TEXT NOT NULL DEFAULT '',
			last_release_title TEXT NOT NULL DEFAULT '',
			last_release_date TEXT NOT NULL DEFAULT '',
			last_checked_at TIMESTAMPTZ,
			UNIQUE (user_id, mbid)
		);
		CREATE TABLE IF NOT EXISTS audit_logs (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
			event TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS audit_logs_created_idx ON audit_logs (created_at DESC);
	`)
	return err
}

func (application *app) ensureAdmin(ctx context.Context) error {
	username := env("ADMIN_USERNAME", "admin")
	password := env("ADMIN_PASSWORD", "change-me")
	var exists bool
	if err := application.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users)").Scan(&exists); err != nil || exists {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = application.db.Exec(ctx, "INSERT INTO users (username, password_hash, is_admin) VALUES ($1, $2, TRUE)", username, string(hash))
	return err
}

func (application *app) scan(ctx context.Context) error {
	application.scanLock.Lock()
	defer application.scanLock.Unlock()
	if err := os.MkdirAll(application.musicDir, 0o755); err != nil {
		return err
	}
	seen := make([]string, 0)
	if err := application.scanRoot(ctx, application.musicDir, "", &seen); err != nil {
		return err
	}
	if err := os.MkdirAll(application.uploadsDir, 0o750); err != nil {
		return err
	}
	if err := application.scanRoot(ctx, application.uploadsDir, "Загрузки", &seen); err != nil {
		return err
	}
	rows, err := application.db.Query(ctx, "SELECT id, name FROM music_sources WHERE remote_name = 'native' ORDER BY id")
	if err != nil {
		return err
	}
	type sourceRoot struct {
		id   int64
		name string
	}
	sources := []sourceRoot{}
	for rows.Next() {
		var source sourceRoot
		if err := rows.Scan(&source.id, &source.name); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, source)
	}
	rows.Close()
	for _, source := range sources {
		if _, err := os.Stat(application.sourceDir(source.id)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := application.scanRoot(ctx, application.sourceDir(source.id), "Облако/"+source.name, &seen); err != nil {
			return err
		}
	}
	if len(seen) == 0 {
		_, err = application.db.Exec(ctx, "DELETE FROM tracks")
		return err
	}
	_, err = application.db.Exec(ctx, "DELETE FROM tracks WHERE NOT (path = ANY($1))", seen)
	return err
}

func (application *app) scanRoot(ctx context.Context, root, prefix string, seen *[]string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !audioExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relativePath = filepath.ToSlash(relativePath)
		libraryPath := relativePath
		if prefix != "" {
			libraryPath = prefix + "/" + relativePath
		}
		*seen = append(*seen, libraryPath)
		var current bool
		err = application.db.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM tracks WHERE path = $1 AND size = $2 AND modified_at = $3 AND metadata_version = 1
		)`, libraryPath, info.Size(), info.ModTime()).Scan(&current)
		if err != nil || current {
			return err
		}
		metadata := readTrackMetadata(ctx, path, relativePath)
		_, err = application.db.Exec(ctx, `
			INSERT INTO tracks (path, title, artist, artists, album, album_artist, year, track_number,
				disc_number, genre, composer, comment, duration, bitrate, sample_rate, format, size,
				modified_at, raw_metadata, cover, cover_mime, metadata_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
				$17, $18, $19, $20, $21, 1)
			ON CONFLICT (path) DO UPDATE SET
				title = CASE WHEN tracks.metadata_edited THEN tracks.title ELSE EXCLUDED.title END,
				artist = CASE WHEN tracks.metadata_edited THEN tracks.artist ELSE EXCLUDED.artist END,
				artists = CASE WHEN tracks.metadata_edited THEN tracks.artists ELSE EXCLUDED.artists END,
				album = CASE WHEN tracks.metadata_edited THEN tracks.album ELSE EXCLUDED.album END,
				album_artist = CASE WHEN tracks.metadata_edited THEN tracks.album_artist ELSE EXCLUDED.album_artist END,
				year = CASE WHEN tracks.metadata_edited THEN tracks.year ELSE EXCLUDED.year END,
				track_number = CASE WHEN tracks.metadata_edited THEN tracks.track_number ELSE EXCLUDED.track_number END,
				disc_number = CASE WHEN tracks.metadata_edited THEN tracks.disc_number ELSE EXCLUDED.disc_number END,
				genre = CASE WHEN tracks.metadata_edited THEN tracks.genre ELSE EXCLUDED.genre END,
				composer = CASE WHEN tracks.metadata_edited THEN tracks.composer ELSE EXCLUDED.composer END,
				comment = CASE WHEN tracks.metadata_edited THEN tracks.comment ELSE EXCLUDED.comment END,
				duration = EXCLUDED.duration, bitrate = EXCLUDED.bitrate, sample_rate = EXCLUDED.sample_rate,
				format = EXCLUDED.format, size = EXCLUDED.size, modified_at = EXCLUDED.modified_at,
				raw_metadata = EXCLUDED.raw_metadata,
				cover = CASE WHEN tracks.metadata_edited THEN tracks.cover ELSE EXCLUDED.cover END,
				cover_mime = CASE WHEN tracks.metadata_edited THEN tracks.cover_mime ELSE EXCLUDED.cover_mime END,
				metadata_version = 1, indexed_at = NOW()`,
			libraryPath, metadata.Title, metadata.Artist, metadata.Artists, metadata.Album,
			metadata.AlbumArtist, metadata.Year, metadata.TrackNumber, metadata.DiscNumber, metadata.Genre,
			metadata.Composer, metadata.Comment, metadata.Duration, metadata.Bitrate, metadata.SampleRate,
			metadata.Format, info.Size(), info.ModTime(), string(metadata.Raw), metadata.Cover, metadata.CoverMIME)
		return err
	})
}

func (application *app) scanLoop(ctx context.Context) {
	interval, err := time.ParseDuration(env("SCAN_INTERVAL", "15m"))
	if err != nil || interval < time.Minute {
		interval = 15 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncErr := application.syncSources(ctx)
			scanErr := application.scan(ctx)
			if syncErr != nil || scanErr != nil {
				log.Printf("автоматическое сканирование: %v", errors.Join(syncErr, scanErr))
				go application.notifyAll(context.Background(), "Ошибка сканирования", "Проверьте состояние музыкального хранилища")
			} else {
				go application.notifyAll(context.Background(), "Сканирование завершено", "Музыкальная библиотека обновлена")
			}
		}
	}
}

func readTrackMetadata(ctx context.Context, path, relativePath string) trackMetadata {
	parts := strings.Split(relativePath, "/")
	result := trackMetadata{
		Title:  strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Format: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
	}
	if len(parts) > 1 {
		result.Artist = parts[0]
		result.Artists = []string{result.Artist}
	}
	if len(parts) > 2 {
		result.Album = parts[len(parts)-2]
	}
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		if metadata, readErr := tag.ReadFrom(file); readErr == nil {
			result.Title = fallback(metadata.Title(), result.Title)
			result.Artist = fallback(metadata.Artist(), result.Artist)
			result.Artists = splitArtists(result.Artist)
			result.Album = fallback(metadata.Album(), result.Album)
			result.AlbumArtist = metadata.AlbumArtist()
			result.Year = metadata.Year()
			result.TrackNumber, _ = metadata.Track()
			result.DiscNumber, _ = metadata.Disc()
			result.Genre = metadata.Genre()
			result.Composer = metadata.Composer()
			result.Comment = metadata.Comment()
			if picture := metadata.Picture(); picture != nil {
				result.Cover = picture.Data
				result.CoverMIME = picture.MIMEType
			}
			result.Raw, _ = json.Marshal(cleanRawTags(metadata.Raw()))
		}
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
			Bitrate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			SampleRate string `json:"sample_rate"`
			Bitrate    string `json:"bit_rate"`
		} `json:"streams"`
	}
	output, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration,bit_rate:stream=sample_rate,bit_rate", "-of", "json", path).Output()
	if err == nil && json.Unmarshal(output, &probe) == nil {
		result.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
		result.Bitrate, _ = strconv.Atoi(probe.Format.Bitrate)
		for _, stream := range probe.Streams {
			if result.SampleRate == 0 {
				result.SampleRate, _ = strconv.Atoi(stream.SampleRate)
			}
			if result.Bitrate == 0 {
				result.Bitrate, _ = strconv.Atoi(stream.Bitrate)
			}
		}
	}
	if len(result.Artists) == 0 && result.Artist != "" {
		result.Artists = []string{result.Artist}
	}
	if len(result.Raw) == 0 {
		result.Raw = []byte("{}")
	}
	return result
}

func splitArtists(value string) []string {
	parts := strings.FieldsFunc(value, func(char rune) bool { return char == ';' || char == '／' })
	result := make([]string, 0, len(parts))
	for _, item := range parts {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func cleanRawTags(raw map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(raw))
	for key, value := range raw {
		switch value.(type) {
		case string, float64, int, int64, bool, []string:
			result[key] = value
		}
	}
	return result
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallbackValue
}

func (application *app) login(response http.ResponseWriter, request *http.Request) {
	clientIP := requestIP(request)
	if !application.loginAllowed(clientIP) {
		writeError(response, http.StatusTooManyRequests, "Слишком много попыток. Повторите через минуту")
		return
	}
	var credentials struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceName string `json:"deviceName"`
	}
	if !decodeJSON(response, request, &credentials) {
		return
	}
	if !usernameValid(strings.TrimSpace(credentials.Username)) || !passwordValid(credentials.Password) {
		application.recordLoginFailure(clientIP)
		writeError(response, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}
	credentials.DeviceName = strings.TrimSpace(credentials.DeviceName)
	if len(credentials.DeviceName) > 80 {
		writeError(response, http.StatusBadRequest, "Некорректное имя устройства")
		return
	}
	var currentUser user
	var passwordHash string
	err := application.db.QueryRow(request.Context(), "SELECT id, username, password_hash, is_admin, theme_id, color_mode FROM users WHERE username = $1", strings.TrimSpace(credentials.Username)).Scan(
		&currentUser.ID, &currentUser.Username, &passwordHash, &currentUser.IsAdmin, &currentUser.Theme, &currentUser.ColorMode,
	)
	if err != nil {
		passwordHash = dummyPasswordHash
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(credentials.Password)) != nil || err != nil {
		application.recordLoginFailure(clientIP)
		writeError(response, http.StatusUnauthorized, "Неверный логин или пароль")
		return
	}
	application.clearLoginFailures(clientIP)
	token, err := application.createSession(request.Context(), currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось создать сессию")
		return
	}
	setSessionCookie(response, request, token, 30*24*60*60)
	if credentials.DeviceName != "" {
		currentUser.Token = token
	}
	writeJSON(response, http.StatusOK, currentUser)
}

func (application *app) logout(response http.ResponseWriter, request *http.Request, _ user) {
	if token := sessionToken(request); token != "" {
		_, _ = application.db.Exec(request.Context(), "DELETE FROM sessions WHERE token_hash = $1", hashToken(token))
	}
	setSessionCookie(response, request, "", -1)
	response.WriteHeader(http.StatusNoContent)
}

func (_ *app) me(response http.ResponseWriter, _ *http.Request, currentUser user) {
	writeJSON(response, http.StatusOK, currentUser)
}

func (application *app) library(response http.ResponseWriter, request *http.Request, currentUser user) {
	requestedPath := cleanLibraryPath(request.URL.Query().Get("path"))
	recursive := request.URL.Query().Get("recursive") == "1"
	prefix := ""
	if requestedPath != "" {
		prefix = requestedPath + "/"
	}
	sortOrder := map[string]string{
		"album":  "t.album, t.disc_number, t.track_number, t.title",
		"artist": "t.artist, t.album, t.disc_number, t.track_number, t.title",
		"title":  "t.title, t.artist",
		"year":   "t.year DESC, t.album, t.track_number, t.title",
	}[request.URL.Query().Get("sort")]
	if sortOrder == "" {
		sortOrder = "t.path"
	}
	format := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("format")))
	genre := strings.TrimSpace(request.URL.Query().Get("genre"))
	rows, err := application.db.Query(request.Context(), `SELECT `+trackSelectColumns+`
		FROM tracks t LEFT JOIN favorites f ON f.track_id = t.id AND f.user_id = $1
		WHERE t.path LIKE $2 AND ($3 = '' OR t.format = $3) AND ($4 = '' OR t.genre = $4)
		ORDER BY `+sortOrder, currentUser.ID, prefix+"%", format, genre)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить библиотеку")
		return
	}
	defer rows.Close()
	result := libraryResponse{Path: requestedPath, Folders: []folder{}, Tracks: []track{}}
	folders := map[string]bool{}
	for rows.Next() {
		var item track
		if err := scanTrack(rows, &item); err != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать библиотеку")
			return
		}
		remainder := strings.TrimPrefix(item.Path, prefix)
		if slash := strings.IndexByte(remainder, '/'); slash >= 0 && !recursive {
			name := remainder[:slash]
			if !folders[name] {
				folders[name] = true
				result.Folders = append(result.Folders, folder{Name: name, Path: prefix + name})
			}
			continue
		}
		result.Tracks = append(result.Tracks, item)
	}
	writeJSON(response, http.StatusOK, result)
}

func (application *app) search(response http.ResponseWriter, request *http.Request, currentUser user) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if query == "" {
		writeJSON(response, http.StatusOK, []track{})
		return
	}
	rows, err := application.db.Query(request.Context(), `SELECT `+trackSelectColumns+`
		FROM tracks t LEFT JOIN favorites f ON f.track_id = t.id AND f.user_id = $1
		WHERE t.title ILIKE $2 OR t.artist ILIKE $2 OR t.album ILIKE $2 OR t.genre ILIKE $2
		ORDER BY t.artist, t.album, t.title LIMIT 100`, currentUser.ID, "%"+query+"%")
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Поиск недоступен")
		return
	}
	defer rows.Close()
	items := []track{}
	for rows.Next() {
		var item track
		if err := scanTrack(rows, &item); err != nil {
			writeError(response, http.StatusInternalServerError, "Ошибка поиска")
			return
		}
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func scanTrack(row interface{ Scan(...any) error }, item *track) error {
	return row.Scan(&item.ID, &item.Title, &item.Artist, &item.Artists, &item.Album, &item.AlbumArtist,
		&item.Year, &item.TrackNumber, &item.DiscNumber, &item.Genre, &item.Composer, &item.Comment,
		&item.Duration, &item.Bitrate, &item.SampleRate, &item.Path, &item.Format, &item.HasCover, &item.Favorite)
}

func (application *app) stream(response http.ResponseWriter, request *http.Request, _ user) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		writeError(response, http.StatusBadRequest, "Некорректный трек")
		return
	}
	var relativePath string
	if err := application.db.QueryRow(request.Context(), "SELECT path FROM tracks WHERE id = $1", id).Scan(&relativePath); err != nil {
		writeError(response, http.StatusNotFound, "Трек не найден")
		return
	}
	fullPath, ok := application.trackPath(request.Context(), relativePath)
	if !ok {
		writeError(response, http.StatusBadRequest, "Некорректный путь")
		return
	}
	file, err := os.Open(fullPath)
	if err != nil {
		writeError(response, http.StatusNotFound, "Файл недоступен")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось прочитать файл")
		return
	}
	response.Header().Set("Content-Disposition", "inline")
	if contentType := mime.TypeByExtension(filepath.Ext(fullPath)); contentType != "" {
		response.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(response, request, info.Name(), info.ModTime(), file)
}

func (application *app) trackPath(ctx context.Context, relativePath string) (string, bool) {
	if strings.HasPrefix(relativePath, "Загрузки/") {
		return safeMusicPath(application.uploadsDir, strings.TrimPrefix(relativePath, "Загрузки/"))
	}
	parts := strings.SplitN(relativePath, "/", 3)
	if len(parts) == 3 && parts[0] == "Облако" {
		var id int64
		if application.db.QueryRow(ctx, "SELECT id FROM music_sources WHERE name = $1 AND remote_name = 'native'", parts[1]).Scan(&id) != nil {
			return "", false
		}
		return safeMusicPath(application.sourceDir(id), parts[2])
	}
	return safeMusicPath(application.musicDir, relativePath)
}

func (application *app) cover(response http.ResponseWriter, request *http.Request, _ user) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		writeError(response, http.StatusBadRequest, "Некорректный трек")
		return
	}
	var data []byte
	var contentType string
	if err := application.db.QueryRow(request.Context(), "SELECT cover, cover_mime FROM tracks WHERE id = $1 AND cover IS NOT NULL", id).Scan(&data, &contentType); err != nil {
		writeError(response, http.StatusNotFound, "Обложка не найдена")
		return
	}
	response.Header().Set("Content-Type", fallback(contentType, "image/jpeg"))
	response.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = response.Write(data)
}

func (application *app) favorite(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		writeError(response, http.StatusBadRequest, "Некорректный трек")
		return
	}
	var payload struct {
		Favorite bool `json:"favorite"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	if payload.Favorite {
		_, err = application.db.Exec(request.Context(), "INSERT INTO favorites (user_id, track_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", currentUser.ID, id)
	} else {
		_, err = application.db.Exec(request.Context(), "DELETE FROM favorites WHERE user_id = $1 AND track_id = $2", currentUser.ID, id)
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось изменить избранное")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) rescan(response http.ResponseWriter, request *http.Request, _ user) {
	syncErr := application.syncSources(request.Context())
	scanErr := application.scan(request.Context())
	if syncErr != nil || scanErr != nil {
		go application.notifyAll(context.Background(), "Ошибка сканирования", "Проверьте состояние музыкального хранилища")
		writeError(response, http.StatusInternalServerError, "Сканирование завершилось с ошибкой")
		return
	}
	go application.notifyAll(context.Background(), "Сканирование завершено", "Музыкальная библиотека обновлена")
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) uploadMusic(response http.ResponseWriter, request *http.Request, currentUser user) {
	uploadDir := application.musicDir
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		writeError(response, http.StatusInternalServerError, "Папка музыки недоступна для записи")
		return
	}
	application.saveUploads(response, request, currentUser, uploadDir)
}

func safeUploadName(value string) string {
	value = filepath.Base(strings.ReplaceAll(value, "\\", "/"))
	if value == "." || value == "" || len(value) > 240 || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return value
}

func availableUploadPath(root, name string) string {
	path := filepath.Join(root, name)
	for index := 2; ; index++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path
		}
		extension := filepath.Ext(name)
		path = filepath.Join(root, strings.TrimSuffix(name, extension)+"-"+strconv.Itoa(index)+extension)
	}
}

type authenticatedHandler func(http.ResponseWriter, *http.Request, user)

func (application *app) auth(next authenticatedHandler) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		token := sessionToken(request)
		if token == "" {
			writeError(response, http.StatusUnauthorized, "Требуется авторизация")
			return
		}
		var currentUser user
		err := application.db.QueryRow(request.Context(), `
			SELECT u.id, u.username, u.is_admin, u.theme_id, u.color_mode FROM sessions s
			JOIN users u ON u.id = s.user_id
			WHERE s.token_hash = $1 AND s.expires_at > NOW()`, hashToken(token)).Scan(&currentUser.ID, &currentUser.Username, &currentUser.IsAdmin, &currentUser.Theme, &currentUser.ColorMode)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(response, http.StatusUnauthorized, "Сессия истекла")
			return
		}
		if err != nil {
			writeError(response, http.StatusInternalServerError, "Ошибка авторизации")
			return
		}
		next(response, request, currentUser)
	}
}

func sessionToken(request *http.Request) string {
	authorization := request.Header.Get("Authorization")
	if strings.HasPrefix(authorization, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		if len(token) == 64 {
			return token
		}
		return ""
	}
	cookie, err := request.Cookie("session")
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (application *app) authAdmin(next authenticatedHandler) http.HandlerFunc {
	return application.auth(func(response http.ResponseWriter, request *http.Request, currentUser user) {
		if !currentUser.IsAdmin {
			writeError(response, http.StatusForbidden, "Недостаточно прав")
			return
		}
		tracked := &statusResponse{ResponseWriter: response, status: http.StatusOK}
		next(tracked, request, currentUser)
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			application.audit(request.Context(), currentUser.ID, fmt.Sprintf("%s %s → %d", request.Method, request.URL.Path, tracked.status))
		}
	})
}

type statusResponse struct {
	http.ResponseWriter
	status int
}

func (response *statusResponse) WriteHeader(status int) {
	response.status = status
	response.ResponseWriter.WriteHeader(status)
}

func cleanLibraryPath(value string) string {
	value = filepath.ToSlash(filepath.Clean("/" + value))
	return strings.Trim(value, "/")
}

func safeMusicPath(root, relativePath string) (string, bool) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", false
	}
	fullPath, err := filepath.Abs(filepath.Join(absoluteRoot, filepath.FromSlash(relativePath)))
	if err != nil {
		return "", false
	}
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(realRoot, realPath)
	return realPath, err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, "Некорректные данные")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(response, http.StatusBadRequest, "Некорректные данные")
		return false
	}
	return true
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Referrer-Policy", "same-origin")
		response.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if strings.HasPrefix(request.URL.Path, "/api/") {
			response.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(response, request)
	})
}

func csrfProtection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions {
			if request.Header.Get("Sec-Fetch-Site") == "cross-site" || !sameOrigin(request) {
				writeError(response, http.StatusForbidden, "Запрос с другого сайта запрещён")
				return
			}
		}
		next.ServeHTTP(response, request)
	})
}

func sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	scheme := "http"
	if request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return err == nil && parsed.Host == request.Host && parsed.Scheme == scheme
}

func requestIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}

func (application *app) loginAllowed(ip string) bool {
	application.loginLock.Lock()
	defer application.loginLock.Unlock()
	attempt := application.loginAttempts[ip]
	if time.Since(attempt.since) >= time.Minute {
		delete(application.loginAttempts, ip)
		return true
	}
	return attempt.count < 10
}

func (application *app) recordLoginFailure(ip string) {
	application.loginLock.Lock()
	defer application.loginLock.Unlock()
	if len(application.loginAttempts) > 10000 {
		for key, value := range application.loginAttempts {
			if time.Since(value.since) >= time.Minute {
				delete(application.loginAttempts, key)
			}
		}
	}
	attempt := application.loginAttempts[ip]
	if time.Since(attempt.since) >= time.Minute {
		attempt = loginAttempt{since: time.Now()}
	}
	attempt.count++
	application.loginAttempts[ip] = attempt
}

func (application *app) clearLoginFailures(ip string) {
	application.loginLock.Lock()
	defer application.loginLock.Unlock()
	delete(application.loginAttempts, ip)
}

func (application *app) createSession(ctx context.Context, userID int64) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = application.db.Exec(ctx, "INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)", hashToken(token), userID, time.Now().Add(30*24*time.Hour))
	return token, err
}

func setSessionCookie(response http.ResponseWriter, request *http.Request, token string, maxAge int) {
	secure := request.TLS != nil || strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https") || env("SECURE_COOKIES", "false") == "true"
	http.SetCookie(response, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge})
}

func usernameValid(value string) bool {
	if len(value) < 3 || len(value) > 64 || value != strings.TrimSpace(value) {
		return false
	}
	for _, char := range value {
		if char < 32 || char == 127 {
			return false
		}
	}
	return true
}

func passwordValid(value string) bool {
	return len(value) >= 8 && len(value) <= 72
}

var dummyPasswordHash = func() string {
	hash, _ := bcrypt.GenerateFromPassword([]byte("invalid-password"), bcrypt.DefaultCost)
	return string(hash)
}()
