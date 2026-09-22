package main

import (
	"bytes"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type trackMetadataForm struct {
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
}

func (application *app) updateTrackMetadata(response http.ResponseWriter, request *http.Request, _ user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 10<<20)
	if err := request.ParseMultipartForm(10 << 20); err != nil {
		writeError(response, http.StatusBadRequest, "Слишком большой файл или некорректные данные")
		return
	}
	defer request.MultipartForm.RemoveAll()
	payload, valid := parseTrackMetadataForm(request)
	if !valid {
		writeError(response, http.StatusBadRequest, "Проверьте название, исполнителей, год и номера трека")
		return
	}
	coverChanged := request.FormValue("removeCover") == "true"
	var coverData []byte
	var coverMIME string
	file, _, fileErr := request.FormFile("cover")
	if fileErr == nil {
		defer file.Close()
		coverData, fileErr = io.ReadAll(file)
		coverMIME, valid = trackCoverValid(coverData)
		coverChanged = true
	}
	if fileErr != nil && fileErr != http.ErrMissingFile || coverChanged && len(coverData) > 0 && !valid {
		writeError(response, http.StatusBadRequest, "Нужна JPEG или PNG от 64 до 4096 px")
		return
	}
	result, err := application.db.Exec(request.Context(), `UPDATE tracks SET title = $1, artist = $2, artists = $3,
		album = $4, album_artist = $5, year = $6, track_number = $7, disc_number = $8, genre = $9,
		composer = $10, comment = $11, cover = CASE WHEN $12 THEN $13 ELSE cover END,
		cover_mime = CASE WHEN $12 THEN $14 ELSE cover_mime END, metadata_edited = TRUE, indexed_at = NOW()
		WHERE id = $15`, payload.Title, payload.Artist, payload.Artists, payload.Album, payload.AlbumArtist,
		payload.Year, payload.TrackNumber, payload.DiscNumber, payload.Genre, payload.Composer, payload.Comment,
		coverChanged, coverData, coverMIME, id)
	if err != nil || result.RowsAffected() == 0 {
		writeError(response, http.StatusNotFound, "Трек не найден")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"id": id, "title": payload.Title, "artist": payload.Artist, "artists": payload.Artists,
		"album": payload.Album, "albumArtist": payload.AlbumArtist, "year": payload.Year,
		"trackNumber": payload.TrackNumber, "discNumber": payload.DiscNumber, "genre": payload.Genre,
		"composer": payload.Composer, "comment": payload.Comment,
		"coverChanged": coverChanged, "hasCover": coverChanged && len(coverData) > 0,
	})
}

func parseTrackMetadataForm(request *http.Request) (trackMetadataForm, bool) {
	result := trackMetadataForm{
		Title: strings.TrimSpace(request.FormValue("title")), Album: strings.TrimSpace(request.FormValue("album")),
		AlbumArtist: strings.TrimSpace(request.FormValue("albumArtist")), Genre: strings.TrimSpace(request.FormValue("genre")),
		Composer: strings.TrimSpace(request.FormValue("composer")), Comment: strings.TrimSpace(request.FormValue("comment")),
	}
	result.Artists = splitArtists(request.FormValue("artists"))
	result.Artist = strings.Join(result.Artists, "; ")
	var ok bool
	if result.Year, ok = metadataNumber(request.FormValue("year"), 9999); !ok {
		return result, false
	}
	if result.TrackNumber, ok = metadataNumber(request.FormValue("trackNumber"), 9999); !ok {
		return result, false
	}
	if result.DiscNumber, ok = metadataNumber(request.FormValue("discNumber"), 999); !ok {
		return result, false
	}
	valid := textLength(result.Title, 1, 300) && textLength(result.Artist, 0, 500) && len(result.Artists) <= 20 &&
		textLength(result.Album, 0, 300) && textLength(result.AlbumArtist, 0, 300) && textLength(result.Genre, 0, 200) &&
		textLength(result.Composer, 0, 300) && textLength(result.Comment, 0, 4000)
	for _, artist := range result.Artists {
		valid = valid && textLength(artist, 1, 200)
	}
	return result, valid
}

func metadataNumber(value string, maximum int) (int, bool) {
	if value = strings.TrimSpace(value); value == "" {
		return 0, true
	}
	number, err := strconv.Atoi(value)
	return number, err == nil && number >= 0 && number <= maximum
}

func textLength(value string, minimum, maximum int) bool {
	length := len([]rune(value))
	return length >= minimum && length <= maximum
}

func trackCoverValid(data []byte) (string, bool) {
	contentType := http.DetectContentType(data)
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	return contentType, err == nil && (contentType == "image/jpeg" || contentType == "image/png") &&
		config.Width >= 64 && config.Height >= 64 && config.Width <= 4096 && config.Height <= 4096
}
