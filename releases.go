package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type watchedArtist struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	MBID             string `json:"mbid"`
	LastReleaseTitle string `json:"lastReleaseTitle"`
	LastReleaseDate  string `json:"lastReleaseDate"`
}

type musicBrainzRelease struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ReleaseDate string `json:"first-release-date"`
	Type        string `json:"primary-type"`
}

type artistCandidate struct {
	MBID           string `json:"mbid"`
	Name           string `json:"name"`
	Disambiguation string `json:"disambiguation"`
	Type           string `json:"type"`
	Country        string `json:"country"`
	Area           string `json:"area"`
	Begin          string `json:"begin"`
	End            string `json:"end"`
	Score          int    `json:"score"`
	Image          string `json:"image"`
}

func (application *app) watchedArtists(response http.ResponseWriter, request *http.Request, currentUser user) {
	rows, err := application.db.Query(request.Context(), "SELECT id, name, mbid, last_release_title, last_release_date FROM watched_artists WHERE user_id = $1 ORDER BY name", currentUser.ID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось загрузить исполнителей")
		return
	}
	defer rows.Close()
	items := []watchedArtist{}
	for rows.Next() {
		var item watchedArtist
		if rows.Scan(&item.ID, &item.Name, &item.MBID, &item.LastReleaseTitle, &item.LastReleaseDate) != nil {
			writeError(response, http.StatusInternalServerError, "Не удалось прочитать исполнителей")
			return
		}
		items = append(items, item)
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) addWatchedArtist(response http.ResponseWriter, request *http.Request, currentUser user) {
	var payload struct {
		MBID string `json:"mbid"`
	}
	if !decodeJSON(response, request, &payload) {
		return
	}
	payload.MBID = strings.ToLower(strings.TrimSpace(payload.MBID))
	if !mbidValid(payload.MBID) {
		writeError(response, http.StatusBadRequest, "Выберите исполнителя из результатов поиска")
		return
	}
	name, err := artistName(request.Context(), payload.MBID)
	if err != nil || name == "" {
		writeError(response, http.StatusBadGateway, "Исполнитель не найден в MusicBrainz")
		return
	}
	release, err := latestRelease(request.Context(), payload.MBID)
	if err != nil {
		writeError(response, http.StatusBadGateway, "MusicBrainz временно недоступен")
		return
	}
	var id int64
	err = application.db.QueryRow(request.Context(), `INSERT INTO watched_artists (user_id, name, mbid, last_release_id, last_release_title, last_release_date, last_checked_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW()) ON CONFLICT (user_id, mbid) DO UPDATE SET name = EXCLUDED.name RETURNING id`, currentUser.ID, name, payload.MBID, release.ID, release.Title, release.ReleaseDate).Scan(&id)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "Не удалось добавить исполнителя")
		return
	}
	writeJSON(response, http.StatusCreated, watchedArtist{ID: id, Name: name, MBID: payload.MBID, LastReleaseTitle: release.Title, LastReleaseDate: release.ReleaseDate})
}

func (application *app) searchArtists(response http.ResponseWriter, request *http.Request, _ user) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if len(query) < 2 || len(query) > 120 {
		writeError(response, http.StatusBadRequest, "Введите от 2 до 120 символов")
		return
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(query)
	endpoint := "https://musicbrainz.org/ws/2/artist/?fmt=json&limit=5&query=" + url.QueryEscape(`artist:"`+escaped+`"`)
	var result struct {
		Artists []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Disambiguation string `json:"disambiguation"`
			Type           string `json:"type"`
			Country        string `json:"country"`
			Score          int    `json:"score"`
			Area           struct {
				Name string `json:"name"`
			} `json:"area"`
			LifeSpan struct {
				Begin string `json:"begin"`
				End   string `json:"end"`
			} `json:"life-span"`
		} `json:"artists"`
	}
	if err := musicBrainzJSON(request.Context(), endpoint, &result); err != nil {
		writeError(response, http.StatusBadGateway, "MusicBrainz временно недоступен")
		return
	}
	items := make([]artistCandidate, 0, len(result.Artists))
	for _, item := range result.Artists {
		items = append(items, artistCandidate{MBID: item.ID, Name: item.Name, Disambiguation: item.Disambiguation, Type: item.Type, Country: item.Country, Area: item.Area.Name, Begin: item.LifeSpan.Begin, End: item.LifeSpan.End, Score: item.Score, Image: "/api/artists/image?mbid=" + url.QueryEscape(item.ID)})
	}
	writeJSON(response, http.StatusOK, items)
}

func (application *app) artistImage(response http.ResponseWriter, request *http.Request, _ user) {
	mbid := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("mbid")))
	if !mbidValid(mbid) {
		http.NotFound(response, request)
		return
	}
	parameters := url.Values{"action": {"query"}, "list": {"search"}, "srsearch": {"haswbstatement:P434=" + mbid}, "srnamespace": {"0"}, "srlimit": {"1"}, "format": {"json"}}
	var searchResult struct {
		Query struct {
			Search []struct {
				Title string `json:"title"`
			} `json:"search"`
		} `json:"query"`
	}
	if externalJSON(request.Context(), "https://www.wikidata.org/w/api.php?"+parameters.Encode(), &searchResult) != nil || len(searchResult.Query.Search) == 0 || !qidValid(searchResult.Query.Search[0].Title) {
		http.NotFound(response, request)
		return
	}
	qid := searchResult.Query.Search[0].Title
	var entityResult struct {
		Claims map[string][]struct {
			MainSnak struct {
				DataValue struct {
					Value string `json:"value"`
				} `json:"datavalue"`
			} `json:"mainsnak"`
		} `json:"claims"`
	}
	claimParameters := url.Values{"action": {"wbgetclaims"}, "entity": {qid}, "property": {"P18"}, "format": {"json"}}
	if externalJSON(request.Context(), "https://www.wikidata.org/w/api.php?"+claimParameters.Encode(), &entityResult) != nil {
		http.NotFound(response, request)
		return
	}
	images := entityResult.Claims["P18"]
	if len(images) == 0 || images[0].MainSnak.DataValue.Value == "" {
		http.NotFound(response, request)
		return
	}
	imageParameters := url.Values{"action": {"query"}, "prop": {"imageinfo"}, "iiprop": {"url"}, "iiurlwidth": {"320"}, "titles": {"File:" + images[0].MainSnak.DataValue.Value}, "format": {"json"}}
	var imageResult struct {
		Query struct {
			Pages map[string]struct {
				ImageInfo []struct {
					URL      string `json:"url"`
					ThumbURL string `json:"thumburl"`
				} `json:"imageinfo"`
			} `json:"pages"`
		} `json:"query"`
	}
	if externalJSON(request.Context(), "https://commons.wikimedia.org/w/api.php?"+imageParameters.Encode(), &imageResult) != nil {
		http.NotFound(response, request)
		return
	}
	imageSource := ""
	for _, page := range imageResult.Query.Pages {
		if len(page.ImageInfo) > 0 {
			imageSource = page.ImageInfo[0].ThumbURL
			if imageSource == "" {
				imageSource = page.ImageInfo[0].URL
			}
			break
		}
	}
	imageURL, ok := wikimediaImageURL(imageSource)
	if !ok {
		http.NotFound(response, request)
		return
	}
	imageRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, imageURL.String(), nil)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	imageRequest.Header.Set("User-Agent", "Resonyr/0.1 ("+env("PUBLIC_URL", "self-hosted")+")")
	imageResponse, err := externalClient.Do(imageRequest)
	if err != nil || imageResponse.StatusCode != http.StatusOK {
		if imageResponse != nil {
			imageResponse.Body.Close()
		}
		http.NotFound(response, request)
		return
	}
	defer imageResponse.Body.Close()
	data, err := io.ReadAll(io.LimitReader(imageResponse.Body, 3<<20+1))
	contentType := imageResponse.Header.Get("Content-Type")
	if err != nil || len(data) > 3<<20 || !strings.HasPrefix(contentType, "image/") {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = response.Write(data)
}

func (application *app) deleteWatchedArtist(response http.ResponseWriter, request *http.Request, currentUser user) {
	id, ok := parseID(response, request.PathValue("id"))
	if !ok {
		return
	}
	result, err := application.db.Exec(request.Context(), "DELETE FROM watched_artists WHERE id = $1 AND user_id = $2", id, currentUser.ID)
	if err != nil || result.RowsAffected() == 0 {
		writeError(response, http.StatusNotFound, "Исполнитель не найден")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (application *app) releaseLoop(ctx context.Context) {
	interval, err := time.ParseDuration(env("RELEASE_CHECK_INTERVAL", "12h"))
	if err != nil || interval < time.Hour {
		interval = 12 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			application.checkReleases(ctx)
		}
	}
}

func (application *app) checkReleases(ctx context.Context) {
	rows, err := application.db.Query(ctx, "SELECT id, user_id, name, mbid, last_release_id FROM watched_artists ORDER BY id")
	if err != nil {
		return
	}
	type watch struct {
		id, userID         int64
		name, mbid, lastID string
	}
	items := []watch{}
	for rows.Next() {
		var item watch
		if rows.Scan(&item.id, &item.userID, &item.name, &item.mbid, &item.lastID) == nil {
			items = append(items, item)
		}
	}
	rows.Close()
	for _, item := range items {
		release, err := latestRelease(ctx, item.mbid)
		if err == nil && release.ID != "" {
			_, _ = application.db.Exec(ctx, "UPDATE watched_artists SET last_release_id = $2, last_release_title = $3, last_release_date = $4, last_checked_at = NOW() WHERE id = $1", item.id, release.ID, release.Title, release.ReleaseDate)
			if item.lastID != "" && release.ID != item.lastID {
				application.notifyUser(ctx, item.userID, "Новый релиз", fmt.Sprintf("%s выпустили «%s»", item.name, release.Title))
			}
		}
	}
}

func artistName(ctx context.Context, mbid string) (string, error) {
	var result struct {
		Name string `json:"name"`
	}
	err := musicBrainzJSON(ctx, "https://musicbrainz.org/ws/2/artist/"+mbid+"?fmt=json", &result)
	return result.Name, err
}

func latestRelease(ctx context.Context, mbid string) (musicBrainzRelease, error) {
	startDate := fmt.Sprintf("%d-01-01", time.Now().Year()-1)
	query := fmt.Sprintf("arid:%s AND firstreleasedate:[%s TO *]", mbid, startDate)
	endpoint := "https://musicbrainz.org/ws/2/release-group/?fmt=json&limit=100&query=" + url.QueryEscape(query)
	var result struct {
		Releases []musicBrainzRelease `json:"release-groups"`
	}
	if err := musicBrainzJSON(ctx, endpoint, &result); err != nil {
		return musicBrainzRelease{}, err
	}
	return latestDatedRelease(result.Releases, time.Now().Format("2006-01-02")), nil
}

func latestDatedRelease(releases []musicBrainzRelease, today string) musicBrainzRelease {
	latest := musicBrainzRelease{}
	for _, release := range releases {
		if release.ReleaseDate <= today && release.ReleaseDate > latest.ReleaseDate {
			latest = release
		}
	}
	return latest
}

func musicBrainzJSON(ctx context.Context, endpoint string, target any) error {
	musicBrainzLock.Lock()
	defer musicBrainzLock.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if wait := time.Until(musicBrainzLast.Add(1100 * time.Millisecond)); wait > 0 {
			time.Sleep(wait)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("User-Agent", "Resonyr/0.1 ("+env("PUBLIC_URL", "self-hosted")+")")
		response, err := musicBrainzClient.Do(request)
		musicBrainzLast = time.Now()
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusOK {
			err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(target)
			response.Body.Close()
			return err
		}
		status := response.Status
		response.Body.Close()
		if attempt == 0 && (response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable) {
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		return fmt.Errorf("musicbrainz: %s", status)
	}
	return fmt.Errorf("musicbrainz unavailable")
}

func mbidValid(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func qidValid(value string) bool {
	if len(value) < 2 || len(value) > 20 || value[0] != 'Q' {
		return false
	}
	for _, char := range value[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func wikimediaImageURL(value string) (*url.URL, bool) {
	imageURL, err := url.Parse(value)
	return imageURL, err == nil && imageURL.Scheme == "https" && imageURL.Hostname() == "upload.wikimedia.org"
}

func externalJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "Resonyr/0.1 ("+env("PUBLIC_URL", "self-hosted")+")")
	response, err := externalClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("external: %s", response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
}

var (
	musicBrainzClient = &http.Client{Timeout: 15 * time.Second}
	musicBrainzLock   sync.Mutex
	musicBrainzLast   time.Time
	externalClient    = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
)
