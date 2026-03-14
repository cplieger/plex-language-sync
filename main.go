package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	healthFile      = "/tmp/.healthy"
	maxResponseBody = 10 << 20 // 10 MB
	maxCacheSize    = 50 << 20 // 50 MB
	cachePath       = "/config/cache.json"

	defaultUpdateLevel    = sectionTypeShow
	defaultUpdateStrategy = "all"
	defaultScheduleTime   = "02:00"

	userTokenRefreshInterval = 12 * time.Hour
)

// Plex metadata type numbers.
const plexTypeEpisode = 4

// Repeated string literals.
const (
	sectionTypeShow = "show"
	typeEpisode     = "episode"
	stateCreated    = "created"
	stateUpdated    = "updated"
	statePlaying    = "playing"
	levelSeason     = "season"
	strategyNext    = "next"
)

// ---------------------------------------------------------------------------
// Entry point + health probe
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		if _, err := os.Stat(healthFile); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(run())
}

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := loadConfig()
	logConfig(&cfg)

	setHealthy(false)

	client := newPlexClient(cfg.plexURL, cfg.plexToken, cfg.skipTLSVerification)

	// Verify connectivity.
	identity, err := client.getServerIdentity(ctx)
	if err != nil {
		slog.Error("cannot connect to plex server", "error", err)
		return 1
	}
	slog.Info("connected to plex server",
		"name", identity.FriendlyName,
		"id", identity.MachineIdentifier,
		"version", identity.Version)

	// Resolve the admin user.
	adminUser, err := client.getLoggedUser(ctx)
	if err != nil {
		slog.Error("cannot resolve admin user", "error", err)
		return 1
	}
	slog.Info("authenticated as admin user", "name", adminUser.Name, "id", adminUser.ID)

	app := newApp(client, &cfg, identity, adminUser)

	// Load persistent cache.
	if err := app.cache.load(); err != nil {
		slog.Warn("cache load failed, starting fresh", "error", err)
	}

	// Initialize user manager with cached tokens.
	app.users.init(adminUser, client.baseURL, cfg.skipTLSVerification)
	app.users.loadFromCache(&app.cache)

	// Refresh user tokens in background (non-blocking).
	go app.userTokenRefreshLoop(ctx)

	setHealthy(true)
	defer setHealthy(false)
	defer app.cache.save()

	// Start the scheduler.
	go app.runScheduler(ctx)

	// WebSocket listener (blocks until context cancelled).
	app.listen(ctx)

	slog.Info("shutting down", "cause", context.Cause(ctx))
	return 0
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	plexURL             string
	plexToken           string
	updateLevel         string // "show" or "season"
	updateStrategy      string // "all" or "next"
	schedulerTime       string // "HH:MM"
	ignoreLabels        []string
	ignoreLibraries     []string
	triggerOnPlay       bool
	triggerOnScan       bool
	schedulerEnable     bool
	languageProfiles    bool
	debug               bool
	skipTLSVerification bool
}

func loadConfig() config {
	cfg := config{
		plexURL:             requireEnv("PLEX_URL"),
		plexToken:           requireEnv("PLEX_TOKEN"),
		updateLevel:         envOr("UPDATE_LEVEL", defaultUpdateLevel),
		updateStrategy:      envOr("UPDATE_STRATEGY", defaultUpdateStrategy),
		triggerOnPlay:       envBool("TRIGGER_ON_PLAY", true),
		triggerOnScan:       envBool("TRIGGER_ON_SCAN", true),
		schedulerEnable:     envBool("SCHEDULER_ENABLE", true),
		languageProfiles:    envBool("LANGUAGE_PROFILES", true),
		schedulerTime:       envOr("SCHEDULER_SCHEDULE_TIME", defaultScheduleTime),
		debug:               envBool("DEBUG", false),
		skipTLSVerification: envBool("SKIP_TLS_VERIFICATION", false),
	}

	if v := os.Getenv("IGNORE_LABELS"); v != "" {
		cfg.ignoreLabels = splitTrim(v)
	} else {
		cfg.ignoreLabels = []string{"PAL_IGNORE", "PLS_IGNORE"}
	}
	if v := os.Getenv("IGNORE_LIBRARIES"); v != "" {
		cfg.ignoreLibraries = splitTrim(v)
	}

	if cfg.updateLevel != defaultUpdateLevel && cfg.updateLevel != levelSeason {
		slog.Warn("invalid UPDATE_LEVEL, defaulting to show", "value", cfg.updateLevel)
		cfg.updateLevel = defaultUpdateLevel
	}
	if cfg.updateStrategy != defaultUpdateStrategy && cfg.updateStrategy != strategyNext {
		slog.Warn("invalid UPDATE_STRATEGY, defaulting to all", "value", cfg.updateStrategy)
		cfg.updateStrategy = defaultUpdateStrategy
	}

	// Validate scheduler time format.
	if parts := strings.SplitN(cfg.schedulerTime, ":", 2); len(parts) != 2 {
		slog.Warn("invalid SCHEDULER_SCHEDULE_TIME, defaulting", "value", cfg.schedulerTime)
		cfg.schedulerTime = defaultScheduleTime
	} else {
		h, hErr := strconv.Atoi(parts[0])
		m, mErr := strconv.Atoi(parts[1])
		if hErr != nil || mErr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			slog.Warn("invalid SCHEDULER_SCHEDULE_TIME, defaulting", "value", cfg.schedulerTime)
			cfg.schedulerTime = defaultScheduleTime
		}
	}

	if cfg.debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	return cfg
}

func logConfig(cfg *config) {
	slog.Info("configuration loaded",
		"plex_url", cfg.plexURL,
		"plex_token", "configured",
		"update_level", cfg.updateLevel,
		"update_strategy", cfg.updateStrategy,
		"trigger_on_play", cfg.triggerOnPlay,
		"trigger_on_scan", cfg.triggerOnScan,
		"scheduler_enable", cfg.schedulerEnable,
		"language_profiles", cfg.languageProfiles,
		"scheduler_time", cfg.schedulerTime,
		"ignore_labels", cfg.ignoreLabels,
		"ignore_libraries", cfg.ignoreLibraries,
		"debug", cfg.debug,
		"skip_tls_verification", cfg.skipTLSVerification)
}

// ---------------------------------------------------------------------------
// Environment helpers
// ---------------------------------------------------------------------------

func requireEnv(key string) string {
	// Support Docker secrets via _FILE suffix.
	if filePath := os.Getenv(key + "_FILE"); filePath != "" {
		info, err := os.Stat(filePath)
		if err != nil {
			slog.Error("cannot stat secret file", "key", key+"_FILE", "path", filePath, "error", err)
			os.Exit(1)
		}
		if info.Size() > 1<<20 { // 1 MB
			slog.Error("secret file too large", "key", key+"_FILE", "path", filePath, "size", info.Size())
			os.Exit(1)
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			slog.Error("cannot read secret file", "key", key+"_FILE", "path", filePath, "error", err)
			os.Exit(1)
		}
		return strings.TrimSpace(string(data))
	}
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required environment variable is missing", "key", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return fallback
	}
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// setHealthy creates or removes the health marker file.
func setHealthy(ok bool) {
	if ok {
		if f, err := os.Create(healthFile); err == nil {
			f.Close()
		}
	} else {
		os.Remove(healthFile)
	}
}

// ---------------------------------------------------------------------------
// Plex HTTP client
// ---------------------------------------------------------------------------

type plexClient struct {
	httpClient *http.Client
	baseURL    *url.URL
	token      string
}

func newPlexClient(serverURL, token string, skipTLS bool) *plexClient {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		slog.Error("invalid PLEX_URL", "error", err)
		os.Exit(1)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		slog.Error("PLEX_URL must use http or https scheme", "url", serverURL)
		os.Exit(1)
	}
	return &plexClient{baseURL: parsed, token: token, httpClient: newHTTPClient(skipTLS)}
}

// newPlexClientForUser creates a plexClient using a different token but the
// same server base URL and TLS settings.
func newPlexClientForUser(baseURL *url.URL, token string, skipTLS bool) *plexClient {
	return &plexClient{baseURL: baseURL, token: token, httpClient: newHTTPClient(skipTLS)}
}

func newHTTPClient(skipTLS bool) *http.Client {
	c := &http.Client{Timeout: 30 * time.Second}
	if skipTLS {
		c.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return c
}

func (c *plexClient) doJSON(ctx context.Context, method, path string, result any) error {
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method,
		c.baseURL.ResolveReference(ref).String(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		drainBody(resp.Body)
		return errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		// Drain body to allow connection reuse.
		drainBody(resp.Body)
		return fmt.Errorf("plex API %s %s: %s", method, path, resp.Status)
	}
	if result == nil {
		drainBody(resp.Body)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, result)
}

func (c *plexClient) get(ctx context.Context, path string, result any) error {
	return c.doJSON(ctx, http.MethodGet, path, result)
}

func (c *plexClient) put(ctx context.Context, path string) error {
	return c.doJSON(ctx, http.MethodPut, path, nil)
}

var errNotFound = errors.New("not found")

// drainBody reads and discards up to 4 KB to enable HTTP connection reuse.
func drainBody(body io.ReadCloser) {
	if _, err := io.CopyN(io.Discard, body, 4<<10); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("failed to drain response body", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Plex API types
// ---------------------------------------------------------------------------

type mc[T any] struct {
	MediaContainer T `json:"MediaContainer"`
}

type serverIdentity struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
}

type plexAccount struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

type plexUser struct {
	ID   string
	Name string
}

type plexSection struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// plexLabel represents a label tag on a Plex metadata item.
type plexLabel struct {
	Tag string `json:"tag"`
}

type plexEpisode struct {
	RatingKey            string      `json:"ratingKey"`
	ParentRatingKey      string      `json:"parentRatingKey"`
	GrandparentKey       string      `json:"grandparentKey"`
	GrandparentTitle     string      `json:"grandparentTitle"`
	ParentTitle          string      `json:"parentTitle"`
	Title                string      `json:"title"`
	Type                 string      `json:"type"`
	Index                json.Number `json:"index"`
	ParentIndex          json.Number `json:"parentIndex"`
	LibrarySectionID     json.Number `json:"librarySectionID"`
	LibraryTitle         string      `json:"librarySectionTitle"`
	GrandparentRatingKey string      `json:"grandparentRatingKey"`
	Label                []plexLabel `json:"Label"`
	Media                []plexMedia `json:"Media"`
	AddedAt              int64       `json:"addedAt"`
}

func (e *plexEpisode) seasonNum() int {
	n, err := strconv.Atoi(e.ParentIndex.String())
	if err != nil {
		return 0
	}
	return n
}

func (e *plexEpisode) episodeNum() int {
	n, err := strconv.Atoi(e.Index.String())
	if err != nil {
		return 0
	}
	return n
}

func (e *plexEpisode) shortName() string {
	return fmt.Sprintf("'%s' (S%02dE%02d)", e.GrandparentTitle, e.seasonNum(), e.episodeNum())
}

type plexMedia struct {
	Part []plexPart `json:"Part"`
	ID   int        `json:"id"`
}

type plexPart struct {
	Stream []plexStream `json:"Stream"`
	ID     int          `json:"id"`
}

type plexStream struct {
	LanguageCode         string `json:"languageCode"`
	LanguageTag          string `json:"languageTag"`
	DisplayTitle         string `json:"displayTitle"`
	ExtendedDisplayTitle string `json:"extendedDisplayTitle"`
	Title                string `json:"title"`
	Codec                string `json:"codec"`
	AudioChannelLayout   string `json:"audioChannelLayout"`
	ID                   int    `json:"id"`
	StreamType           int    `json:"streamType"` // 1=video, 2=audio, 3=subtitle
	Channels             int    `json:"channels"`
	Selected             bool   `json:"selected"`
	Forced               bool   `json:"forced"`
	HearingImpaired      bool   `json:"hearingImpaired"`
	VisualImpaired       bool   `json:"visualImpaired"`
}

func (s *plexStream) isAudio() bool    { return s.StreamType == 2 }
func (s *plexStream) isSubtitle() bool { return s.StreamType == 3 }

// plexSession represents a single active session from /status/sessions.
type plexSession struct {
	User struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"User"`
	Player struct {
		MachineIdentifier string `json:"machineIdentifier"`
	} `json:"Player"`
}

// sharedServersXML is the XML response from plex.tv shared_servers endpoint.
type sharedServersXML struct {
	XMLName      xml.Name          `xml:"MediaContainer"`
	SharedServer []sharedServerXML `xml:"SharedServer"`
}

type sharedServerXML struct {
	UserID      string `xml:"userID,attr"`
	Username    string `xml:"username,attr"`
	AccessToken string `xml:"accessToken,attr"`
}

type plexHistoryItem struct {
	RatingKey        string      `json:"ratingKey"`
	Type             string      `json:"type"`
	AccountID        json.Number `json:"accountID"`
	LibrarySectionID json.Number `json:"librarySectionID"`
	LibraryTitle     string      `json:"librarySectionTitle"`
}

// ---------------------------------------------------------------------------
// Plex API methods
// ---------------------------------------------------------------------------

func (c *plexClient) getServerIdentity(ctx context.Context) (*serverIdentity, error) {
	var resp mc[serverIdentity]
	if err := c.get(ctx, "/", &resp); err != nil {
		return nil, err
	}
	return &resp.MediaContainer, nil
}

func (c *plexClient) getLoggedUser(ctx context.Context) (*plexUser, error) {
	// Get the admin username from myPlex account.
	var acctResp struct {
		Username string `json:"username"`
	}
	if err := c.get(ctx, "/myplex/account", &acctResp); err != nil {
		return nil, fmt.Errorf("fetching account: %w", err)
	}
	// Match against system accounts.
	var sysResp mc[struct {
		Account []plexAccount `json:"Account"`
	}]
	if err := c.get(ctx, "/accounts", &sysResp); err != nil {
		return nil, fmt.Errorf("fetching system accounts: %w", err)
	}
	for _, a := range sysResp.MediaContainer.Account {
		if a.Name == acctResp.Username {
			return &plexUser{ID: strconv.Itoa(a.ID), Name: a.Name}, nil
		}
	}
	return nil, fmt.Errorf("admin user %q not found in system accounts", acctResp.Username)
}

func (c *plexClient) getShowSections(ctx context.Context) ([]plexSection, error) {
	var resp mc[struct {
		Directory []plexSection `json:"Directory"`
	}]
	if err := c.get(ctx, "/library/sections", &resp); err != nil {
		return nil, err
	}
	var shows []plexSection
	for _, s := range resp.MediaContainer.Directory {
		if s.Type == sectionTypeShow {
			shows = append(shows, s)
		}
	}
	return shows, nil
}

func (c *plexClient) getEpisode(ctx context.Context, ratingKey string) (*plexEpisode, error) {
	return c.getMetadataByKey(ctx, ratingKey)
}

func (c *plexClient) getMetadataByKey(ctx context.Context, ratingKey string) (*plexEpisode, error) {
	if _, err := strconv.Atoi(ratingKey); err != nil {
		return nil, fmt.Errorf("invalid rating key %q", ratingKey)
	}
	var resp mc[struct {
		Metadata []plexEpisode `json:"Metadata"`
	}]
	if err := c.get(ctx, "/library/metadata/"+ratingKey, &resp); err != nil {
		return nil, err
	}
	if len(resp.MediaContainer.Metadata) == 0 {
		return nil, errNotFound
	}
	return &resp.MediaContainer.Metadata[0], nil
}

func (c *plexClient) getShowEpisodes(ctx context.Context, showRatingKey string) ([]plexEpisode, error) {
	if _, err := strconv.Atoi(showRatingKey); err != nil {
		return nil, fmt.Errorf("invalid show rating key %q", showRatingKey)
	}
	var resp mc[struct {
		Metadata []plexEpisode `json:"Metadata"`
	}]
	if err := c.get(ctx, "/library/metadata/"+showRatingKey+"/allLeaves", &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

func (c *plexClient) getSeasonEpisodes(ctx context.Context, seasonRatingKey string) ([]plexEpisode, error) {
	if _, err := strconv.Atoi(seasonRatingKey); err != nil {
		return nil, fmt.Errorf("invalid season rating key %q", seasonRatingKey)
	}
	var resp mc[struct {
		Metadata []plexEpisode `json:"Metadata"`
	}]
	if err := c.get(ctx, "/library/metadata/"+seasonRatingKey+"/children", &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// getShowMetadata fetches the show-level metadata (including labels) for a show.
func (c *plexClient) getShowMetadata(ctx context.Context, showRatingKey string) (*plexEpisode, error) {
	return c.getMetadataByKey(ctx, showRatingKey)
}

// getRecentlyAdded fetches recently added episodes from a library section.
func (c *plexClient) getRecentlyAdded(ctx context.Context, sectionKey string, sinceUnix int64) ([]plexEpisode, error) {
	if _, err := strconv.Atoi(sectionKey); err != nil {
		return nil, fmt.Errorf("invalid section key %q", sectionKey)
	}
	path := fmt.Sprintf("/library/sections/%s/all?type=%d&sort=addedAt:desc&addedAt>>=%d",
		sectionKey, plexTypeEpisode, sinceUnix)
	var resp mc[struct {
		Metadata []plexEpisode `json:"Metadata"`
	}]
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// setAudioStream sets the selected audio stream for a media part.
func (c *plexClient) setAudioStream(ctx context.Context, partID, streamID int) error {
	path := fmt.Sprintf("/library/parts/%d?audioStreamID=%d&allParts=1", partID, streamID)
	return c.put(ctx, path)
}

// setSubtitleStream sets the selected subtitle stream for a media part.
func (c *plexClient) setSubtitleStream(ctx context.Context, partID, streamID int) error {
	path := fmt.Sprintf("/library/parts/%d?subtitleStreamID=%d&allParts=1", partID, streamID)
	return c.put(ctx, path)
}

// disableSubtitles disables subtitles for a media part.
func (c *plexClient) disableSubtitles(ctx context.Context, partID int) error {
	path := fmt.Sprintf("/library/parts/%d?subtitleStreamID=0&allParts=1", partID)
	return c.put(ctx, path)
}

// getHistory fetches recent play history.
func (c *plexClient) getHistory(ctx context.Context, sinceUnix int64) ([]plexHistoryItem, error) {
	path := fmt.Sprintf("/status/sessions/history/all?sort=viewedAt:desc&viewedAt>>=%d", sinceUnix)
	var resp mc[struct {
		Metadata []plexHistoryItem `json:"Metadata"`
	}]
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// getUserFromSession finds the user associated with a clientIdentifier by
// querying active sessions. Returns the user ID and username.
func (c *plexClient) getUserFromSession(ctx context.Context, clientIdentifier string) (userID, username string, err error) {
	var resp mc[struct {
		Metadata []plexSession `json:"Metadata"`
	}]
	if err := c.get(ctx, "/status/sessions", &resp); err != nil {
		return "", "", fmt.Errorf("fetching sessions: %w", err)
	}
	for _, s := range resp.MediaContainer.Metadata {
		if s.Player.MachineIdentifier == clientIdentifier {
			return s.User.ID, s.User.Title, nil
		}
	}
	return "", "", fmt.Errorf("no session found for client %q", clientIdentifier)
}

// getSharedUserTokens fetches shared user tokens from plex.tv.
// This calls the plex.tv API (not the local server).
// Always uses TLS verification for plex.tv regardless of SKIP_TLS_VERIFICATION.
func (c *plexClient) getSharedUserTokens(ctx context.Context, machineIdentifier string) ([]sharedServerXML, error) {
	apiURL := "https://plex.tv/api/servers/" + url.PathEscape(machineIdentifier) + "/shared_servers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("X-Plex-Token", c.token)

	// Use a dedicated client for plex.tv — never skip TLS for public endpoints.
	plexTVClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := plexTVClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex.tv shared_servers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		drainBody(resp.Body)
		return nil, fmt.Errorf("plex.tv shared_servers: %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, err
	}

	var result sharedServersXML
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing shared_servers XML: %w", err)
	}
	return result.SharedServer, nil
}

// ---------------------------------------------------------------------------
// Stream selection helpers
// ---------------------------------------------------------------------------

// selectedStreams returns the currently selected audio and subtitle streams
// from the first part of the first media of an episode.
func selectedStreams(ep *plexEpisode) (audio, subtitle *plexStream) {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil, nil
	}
	for i := range ep.Media[0].Part[0].Stream {
		s := &ep.Media[0].Part[0].Stream[i]
		if s.isAudio() && s.Selected {
			audio = s
		}
		if s.isSubtitle() && s.Selected {
			subtitle = s
		}
	}
	return audio, subtitle
}

// audioStreams returns all audio streams from the first part.
func audioStreams(ep *plexEpisode) []*plexStream {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil
	}
	var out []*plexStream
	for i := range ep.Media[0].Part[0].Stream {
		s := &ep.Media[0].Part[0].Stream[i]
		if s.isAudio() {
			out = append(out, s)
		}
	}
	return out
}

// subtitleStreams returns all subtitle streams from the first part.
func subtitleStreams(ep *plexEpisode) []*plexStream {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil
	}
	var out []*plexStream
	for i := range ep.Media[0].Part[0].Stream {
		s := &ep.Media[0].Part[0].Stream[i]
		if s.isSubtitle() {
			out = append(out, s)
		}
	}
	return out
}

// firstPartID returns the part ID of the first media part.
func firstPartID(ep *plexEpisode) int {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return 0
	}
	return ep.Media[0].Part[0].ID
}

// matchAudioStream finds the best matching audio stream from candidates
// based on a reference stream. Matching logic inspired by Plex-Auto-Languages.
func matchAudioStream(ref *plexStream, candidates []*plexStream) *plexStream {
	if ref == nil {
		return nil
	}
	streams := filterByLanguage(candidates, ref.LanguageCode)
	if len(streams) == 0 {
		return nil
	}
	if len(streams) == 1 {
		return streams[0]
	}

	streams = filterByBoolPref(streams, ref.VisualImpaired,
		func(s *plexStream) bool { return s.VisualImpaired })

	refTitle := strings.ToLower(ref.titleForMatch())
	streams = filterByBoolPref(streams, containsDescriptive(refTitle),
		func(s *plexStream) bool { return containsDescriptive(strings.ToLower(s.titleForMatch())) })

	if len(streams) == 1 {
		return streams[0]
	}
	return bestByScore(streams, func(s *plexStream) int {
		return scoreAudioStream(ref, s)
	})
}

func scoreAudioStream(ref, s *plexStream) int {
	score := 0
	if ref.Codec == s.Codec {
		score += 5
	}
	if ref.AudioChannelLayout == s.AudioChannelLayout {
		score += 3
	}
	// When streams are ambiguous (same titles), prefer more channels
	// if the reference has few channels — avoids descriptive 2.0 tracks.
	if ref.Channels > 0 && s.Channels > 0 {
		if ref.Channels < 3 && s.Channels > ref.Channels {
			score += 2
		}
	}
	score += titleMatchScore(ref, s)
	return score
}

func titleMatchScore(ref, s *plexStream) int {
	score := 0
	if ref.ExtendedDisplayTitle != "" && s.ExtendedDisplayTitle != "" &&
		ref.ExtendedDisplayTitle == s.ExtendedDisplayTitle {
		score += 5
	}
	if ref.DisplayTitle != "" && s.DisplayTitle != "" &&
		ref.DisplayTitle == s.DisplayTitle {
		score += 5
	}
	if ref.Title != "" && s.Title != "" && ref.Title == s.Title {
		score += 5
	}
	return score
}

// filterByLanguage returns streams matching the given language code.
func filterByLanguage(streams []*plexStream, langCode string) []*plexStream {
	var out []*plexStream
	for _, s := range streams {
		if s.LanguageCode == langCode {
			out = append(out, s)
		}
	}
	return out
}

// filterByBoolPref filters streams preferring those matching the desired boolean.
// If no streams match, returns the original list unchanged.
func filterByBoolPref(streams []*plexStream, desired bool, fn func(*plexStream) bool) []*plexStream {
	var matching []*plexStream
	for _, s := range streams {
		if fn(s) == desired {
			matching = append(matching, s)
		}
	}
	if len(matching) > 0 {
		return matching
	}
	return streams
}

// bestByScore returns the stream with the highest score.
func bestByScore(streams []*plexStream, scoreFn func(*plexStream) int) *plexStream {
	bestIdx, bestScore := 0, -1
	for i, s := range streams {
		score := scoreFn(s)
		if score > bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return streams[bestIdx]
}

func matchSubtitleStream(ref, refAudio *plexStream, candidates []*plexStream) *plexStream {
	langCode, matchForcedOnly, matchHIOnly := subtitleMatchCriteria(ref, refAudio)
	if langCode == "" {
		return nil
	}

	streams := filterByLanguage(candidates, langCode)
	if matchForcedOnly {
		// For forced-only, we need exact match — not "prefer".
		var forced []*plexStream
		for _, s := range streams {
			if s.Forced {
				forced = append(forced, s)
			}
		}
		streams = forced
	}
	if matchHIOnly {
		var hi []*plexStream
		for _, s := range streams {
			if s.HearingImpaired {
				hi = append(hi, s)
			}
		}
		streams = hi
	}

	if len(streams) == 0 {
		return nil
	}
	if len(streams) == 1 {
		return streams[0]
	}

	return bestByScore(streams, func(s *plexStream) int {
		return scoreSubtitleStream(ref, s)
	})
}

func subtitleMatchCriteria(ref, refAudio *plexStream) (langCode string, forcedOnly, hiOnly bool) {
	if ref == nil {
		if refAudio == nil {
			return "", false, false
		}
		return refAudio.LanguageCode, true, false
	}
	return ref.LanguageCode, ref.Forced, ref.HearingImpaired
}

func scoreSubtitleStream(ref, s *plexStream) int {
	if ref == nil {
		return 0
	}
	score := 0
	if ref.Forced == s.Forced {
		score += 3
	}
	if ref.HearingImpaired == s.HearingImpaired {
		score += 3
	}
	if ref.Codec != "" && s.Codec != "" && ref.Codec == s.Codec {
		score++
	}
	score += titleMatchScore(ref, s)
	return score
}

func (s *plexStream) titleForMatch() string {
	if s.ExtendedDisplayTitle != "" {
		return s.ExtendedDisplayTitle
	}
	if s.DisplayTitle != "" {
		return s.DisplayTitle
	}
	return s.Title
}

func containsDescriptive(title string) bool {
	for _, term := range []string{
		"commentary", "description", "descriptive",
		"narration", "narrative", "described",
	} {
		if strings.Contains(title, term) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// User management
// ---------------------------------------------------------------------------

type userInfo struct {
	ID    string
	Name  string
	Token string
}

type userManager struct {
	shared  map[string]userInfo    // keyed by userID
	clients map[string]*plexClient // cached per-user clients
	baseURL *url.URL
	admin   userInfo
	mu      sync.Mutex
	skipTLS bool
}

func (um *userManager) init(admin *plexUser, baseURL *url.URL, skipTLS bool) {
	um.mu.Lock()
	defer um.mu.Unlock()
	um.admin = userInfo{ID: admin.ID, Name: admin.Name}
	um.baseURL = baseURL
	um.skipTLS = skipTLS
	if um.shared == nil {
		um.shared = make(map[string]userInfo)
	}
	um.clients = make(map[string]*plexClient)
}

// loadFromCache restores cached user tokens so we don't need plex.tv on restart.
func (um *userManager) loadFromCache(cache *appCache) {
	cache.mu.Lock()
	// Copy tokens under cache lock, then release before touching um.
	tokensCopy := make(map[string]string, len(cache.data.UserTokens))
	maps.Copy(tokensCopy, cache.data.UserTokens)
	cache.mu.Unlock()

	um.mu.Lock()
	defer um.mu.Unlock()
	for uid, token := range tokensCopy {
		if uid == um.admin.ID {
			continue
		}
		if _, exists := um.shared[uid]; !exists {
			um.shared[uid] = userInfo{ID: uid, Token: token, Name: "user-" + uid}
		}
	}
}

// refreshTokens fetches shared user tokens from plex.tv and updates the cache.
func (um *userManager) refreshTokens(ctx context.Context, adminClient *plexClient, machineID string, cache *appCache) {
	servers, err := adminClient.getSharedUserTokens(ctx, machineID)
	if err != nil {
		slog.Warn("failed to refresh shared user tokens", "error", err)
		return
	}

	um.mu.Lock()
	for _, s := range servers {
		if s.UserID == "" || s.AccessToken == "" {
			continue
		}
		um.shared[s.UserID] = userInfo{
			ID:    s.UserID,
			Name:  s.Username,
			Token: s.AccessToken,
		}
	}
	// Copy tokens while holding um.mu, then release before touching cache.
	tokensCopy := make(map[string]string, len(um.shared))
	for uid, info := range um.shared {
		tokensCopy[uid] = info.Token
	}
	um.mu.Unlock()

	// Persist to cache (separate lock scope — no nesting).
	cache.mu.Lock()
	if cache.data.UserTokens == nil {
		cache.data.UserTokens = make(map[string]string)
	}
	maps.Copy(cache.data.UserTokens, tokensCopy)
	cache.mu.Unlock()
	cache.save()

	slog.Info("shared user tokens refreshed", "users", len(servers))
}

// clientForUser returns a plexClient using the given user's token.
// Caches clients to avoid creating new HTTP connection pools on every call.
// Falls back to the admin client if the userID matches admin or is unknown.
func (um *userManager) clientForUser(userID string, adminClient *plexClient) *plexClient {
	um.mu.Lock()
	defer um.mu.Unlock()

	if userID == um.admin.ID {
		return adminClient
	}
	// Return cached client if token hasn't changed.
	if cached, ok := um.clients[userID]; ok {
		if info, exists := um.shared[userID]; exists && cached.token == info.Token {
			return cached
		}
	}
	if info, ok := um.shared[userID]; ok && info.Token != "" {
		c := newPlexClientForUser(um.baseURL, info.Token, um.skipTLS)
		um.clients[userID] = c
		return c
	}
	// Unknown user — fall back to admin.
	return adminClient
}

// allUsers returns the admin plus all shared users.
func (um *userManager) allUsers(adminToken string) []userInfo {
	um.mu.Lock()
	defer um.mu.Unlock()

	users := make([]userInfo, 0, 1+len(um.shared))
	admin := um.admin
	admin.Token = adminToken
	users = append(users, admin)
	for _, u := range um.shared {
		users = append(users, u)
	}
	return users
}

// userName returns the display name for a userID.
func (um *userManager) userName(userID string) string {
	um.mu.Lock()
	defer um.mu.Unlock()
	if userID == um.admin.ID {
		return um.admin.Name
	}
	if info, ok := um.shared[userID]; ok {
		return info.Name
	}
	return "unknown-" + userID
}

// ---------------------------------------------------------------------------
// Persistent cache
// ---------------------------------------------------------------------------

type appCache struct {
	data cacheData
	mu   sync.Mutex
}

type cacheData struct {
	// ProcessedEpisodes tracks recently processed episode keys to avoid
	// re-processing the same episode on rapid successive events.
	// Keys include userID: "play:{userID}:{ratingKey}".
	ProcessedEpisodes map[string]int64 `json:"processed_episodes"`
	// LanguageProfiles maps userID → audioLang → subtitleLang.
	// Empty subtitle string means "no subtitles" for that audio language.
	LanguageProfiles map[string]map[string]string `json:"language_profiles"`
	// UserTokens maps userID → accessToken for shared users.
	UserTokens map[string]string `json:"user_tokens"`
	// LastSchedulerRun is the unix timestamp of the last scheduler run.
	LastSchedulerRun int64 `json:"last_scheduler_run"`
}

func (c *appCache) load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data.ProcessedEpisodes = make(map[string]int64)
	c.data.LanguageProfiles = make(map[string]map[string]string)
	c.data.UserTokens = make(map[string]string)

	f, err := os.Open(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCacheSize))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &c.data)
}

func (c *appCache) save() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneOldEntries()

	data, err := json.MarshalIndent(&c.data, "", "  ")
	if err != nil {
		slog.Warn("cache marshal failed", "error", err)
		return
	}

	// Atomic write: temp file + rename prevents corruption on crash.
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".cache-*.tmp")
	if err != nil {
		slog.Warn("cache temp file failed", "error", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		slog.Warn("cache write failed", "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		slog.Warn("cache close failed", "error", err)
		return
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		os.Remove(tmpName)
		slog.Warn("cache rename failed", "path", cachePath, "error", err)
	}
}

func (c *appCache) wasRecentlyProcessed(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, ok := c.data.ProcessedEpisodes[key]
	if !ok {
		return false
	}
	return time.Since(time.Unix(ts, 0)) < 5*time.Minute
}

func (c *appCache) markProcessed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.ProcessedEpisodes == nil {
		c.data.ProcessedEpisodes = make(map[string]int64)
	}
	c.data.ProcessedEpisodes[key] = time.Now().Unix()
	// Inline prune if map grows too large (>10k entries).
	if len(c.data.ProcessedEpisodes) > 10000 {
		c.pruneOldEntries()
	}
}

func (c *appCache) pruneOldEntries() {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	for k, ts := range c.data.ProcessedEpisodes {
		if ts < cutoff {
			delete(c.data.ProcessedEpisodes, k)
		}
	}
}

// learnLanguageProfile records a user's audio→subtitle language preference.
func (c *appCache) learnLanguageProfile(userID, audioLang, subtitleLang string) {
	if audioLang == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.LanguageProfiles == nil {
		c.data.LanguageProfiles = make(map[string]map[string]string)
	}
	if c.data.LanguageProfiles[userID] == nil {
		c.data.LanguageProfiles[userID] = make(map[string]string)
	}
	prev, exists := c.data.LanguageProfiles[userID][audioLang]
	if !exists || prev != subtitleLang {
		c.data.LanguageProfiles[userID][audioLang] = subtitleLang
		slog.Info("language profile updated",
			"user", userID,
			"audio_lang", audioLang,
			"subtitle_lang", subtitleLang)
	}
}

// getSubtitleLangForAudio returns the learned subtitle language for a given
// audio language and user. Returns ("", false) if no profile exists.
func (c *appCache) getSubtitleLangForAudio(userID, audioLang string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.LanguageProfiles == nil {
		return "", false
	}
	userProfiles, ok := c.data.LanguageProfiles[userID]
	if !ok {
		return "", false
	}
	lang, ok := userProfiles[audioLang]
	return lang, ok
}

// ---------------------------------------------------------------------------
// Application
// ---------------------------------------------------------------------------

type app struct {
	client   *plexClient
	cfg      *config
	identity *serverIdentity
	admin    *plexUser
	cache    appCache
	users    userManager
}

func newApp(client *plexClient, cfg *config, identity *serverIdentity, admin *plexUser) *app {
	return &app{
		client:   client,
		cfg:      cfg,
		identity: identity,
		admin:    admin,
	}
}

// shouldIgnoreLibrary checks if a library title is in the ignore list.
func (a *app) shouldIgnoreLibrary(title string) bool {
	return slices.Contains(a.cfg.ignoreLibraries, title)
}

// hasIgnoreLabel returns true if any of the episode's labels match the ignore list.
func hasIgnoreLabel(labels []plexLabel, ignoreLabels []string) bool {
	for _, label := range labels {
		if slices.Contains(ignoreLabels, label.Tag) {
			return true
		}
	}
	return false
}

// shouldIgnoreShow checks if a show has any of the ignore labels.
// Uses admin client since labels are server-level, not per-user.
func (a *app) shouldIgnoreShow(ctx context.Context, showRatingKey string) bool {
	show, err := a.client.getShowMetadata(ctx, showRatingKey)
	if err != nil {
		return false
	}
	return hasIgnoreLabel(show.Label, a.cfg.ignoreLabels)
}

// userTokenRefreshLoop periodically refreshes shared user tokens from plex.tv.
func (a *app) userTokenRefreshLoop(ctx context.Context) {
	// Initial refresh.
	a.users.refreshTokens(ctx, a.client, a.identity.MachineIdentifier, &a.cache)

	ticker := time.NewTicker(userTokenRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.users.refreshTokens(ctx, a.client, a.identity.MachineIdentifier, &a.cache)
		case <-ctx.Done():
			return
		}
	}
}

// changeTracksForEpisode applies language preferences from a reference episode
// to other episodes in the same show/season, using a per-user client.
func (a *app) changeTracksForEpisode(ctx context.Context, userClient *plexClient, userID string, reference *plexEpisode, trigger string) {
	username := a.users.userName(userID)
	refAudio, refSub := selectedStreams(reference)
	if refAudio == nil {
		slog.Debug("no audio stream selected on reference, skipping",
			"episode", reference.shortName(), "user", username)
		return
	}

	// Learn language profile from the user's active choice.
	if a.cfg.languageProfiles && refAudio.LanguageCode != "" {
		subLang := ""
		if refSub != nil {
			subLang = refSub.LanguageCode
		}
		a.cache.learnLanguageProfile(userID, refAudio.LanguageCode, subLang)
	}

	showRatingKey := reference.GrandparentRatingKey
	if showRatingKey == "" {
		slog.Debug("no show rating key, skipping",
			"episode", reference.shortName(), "user", username)
		return
	}

	// Check ignore rules (admin client — labels/libraries are server-level).
	if a.shouldIgnoreLibrary(reference.LibraryTitle) {
		slog.Debug("library ignored", "library", reference.LibraryTitle)
		return
	}
	if a.shouldIgnoreShow(ctx, showRatingKey) {
		slog.Debug("show ignored", "show", reference.GrandparentTitle)
		return
	}

	// Get episodes to update using the user's client.
	var episodes []plexEpisode
	var err error
	if a.cfg.updateLevel == levelSeason {
		episodes, err = userClient.getSeasonEpisodes(ctx, reference.ParentRatingKey)
	} else {
		episodes, err = userClient.getShowEpisodes(ctx, showRatingKey)
	}
	if err != nil {
		slog.Warn("failed to fetch episodes for update",
			"show", reference.GrandparentTitle, "user", username, "error", err)
		return
	}

	// Filter by strategy.
	if a.cfg.updateStrategy == strategyNext {
		episodes = filterEpisodesAfter(episodes, reference)
	}

	changes := 0
	for i := range episodes {
		ep := &episodes[i]
		if a.updateEpisodeStreams(ctx, userClient, ep.RatingKey, refAudio, refSub) {
			changes++
		}
	}

	if changes > 0 {
		slog.Info("language update complete",
			"trigger", trigger,
			"user", username,
			"show", reference.GrandparentTitle,
			"reference", reference.shortName(),
			"audio", streamDesc(refAudio),
			"subtitle", streamDesc(refSub),
			"episodes_updated", changes,
			"episodes_total", len(episodes))
	}
}

// updateEpisodeStreams applies reference audio/subtitle streams to a single episode
// using the provided per-user client. Returns true if any changes were made.
func (a *app) updateEpisodeStreams(ctx context.Context, userClient *plexClient, ratingKey string, refAudio, refSub *plexStream) bool {
	full, err := userClient.getEpisode(ctx, ratingKey)
	if err != nil {
		slog.Debug("failed to reload episode", "key", ratingKey, "error", err)
		return false
	}

	partID := firstPartID(full)
	if partID == 0 {
		return false
	}

	curAudio, curSub := selectedStreams(full)
	changed := false

	changed = a.applyAudioStream(ctx, userClient, full, partID, refAudio, curAudio) || changed
	changed = a.applySubtitleStream(ctx, userClient, full, partID, refAudio, refSub, curSub) || changed
	return changed
}

func (a *app) applyAudioStream(ctx context.Context, userClient *plexClient, ep *plexEpisode, partID int, ref, cur *plexStream) bool {
	matched := matchAudioStream(ref, audioStreams(ep))
	if matched == nil || (cur != nil && matched.ID == cur.ID) {
		return false
	}
	if err := userClient.setAudioStream(ctx, partID, matched.ID); err != nil {
		slog.Warn("failed to set audio stream",
			"episode", ep.shortName(), "error", err)
		return false
	}
	return true
}

// shouldSkipSubtitleForCommentary returns true if the reference audio is a
// commentary/descriptive track but the target episode has no matching
// commentary audio track — in which case subtitle changes should be skipped.
func shouldSkipSubtitleForCommentary(refAudio *plexStream, targetAudioStreams []*plexStream) bool {
	if refAudio == nil {
		return false
	}
	if !containsDescriptive(strings.ToLower(refAudio.titleForMatch())) {
		return false
	}
	matched := matchAudioStream(refAudio, targetAudioStreams)
	return matched == nil
}

func (a *app) applySubtitleStream(ctx context.Context, userClient *plexClient, ep *plexEpisode, partID int, refAudio, refSub, curSub *plexStream) bool {
	if shouldSkipSubtitleForCommentary(refAudio, audioStreams(ep)) {
		return false
	}

	matched := matchSubtitleStream(refSub, refAudio, subtitleStreams(ep))
	if matched != nil && (curSub == nil || matched.ID != curSub.ID) {
		if err := userClient.setSubtitleStream(ctx, partID, matched.ID); err != nil {
			slog.Warn("failed to set subtitle stream",
				"episode", ep.shortName(), "error", err)
			return false
		}
		return true
	}
	if matched == nil && curSub != nil && refSub == nil {
		if err := userClient.disableSubtitles(ctx, partID); err != nil {
			slog.Warn("failed to disable subtitles",
				"episode", ep.shortName(), "error", err)
			return false
		}
		return true
	}
	return false
}

func filterEpisodesAfter(episodes []plexEpisode, ref *plexEpisode) []plexEpisode {
	refSeason := ref.seasonNum()
	refEp := ref.episodeNum()
	var out []plexEpisode
	for i := range episodes {
		ep := &episodes[i]
		s := ep.seasonNum()
		e := ep.episodeNum()
		if s > refSeason || (s == refSeason && e > refEp) {
			out = append(out, *ep)
		}
	}
	return out
}

func streamDesc(s *plexStream) string {
	if s == nil {
		return "none"
	}
	if t := s.titleForMatch(); t != "" {
		return t
	}
	return fmt.Sprintf("stream-%d", s.ID)
}

// ---------------------------------------------------------------------------
// WebSocket listener
// ---------------------------------------------------------------------------

type wsNotification struct {
	NotificationContainer struct {
		Type                         string            `json:"type"`
		PlaySessionStateNotification []wsPlayEvent     `json:"PlaySessionStateNotification"`
		TimelineEntry                []wsTimelineEntry `json:"TimelineEntry"`
	} `json:"NotificationContainer"`
}

type wsPlayEvent struct {
	SessionKey       string `json:"sessionKey"`
	ClientIdentifier string `json:"clientIdentifier"`
	RatingKey        string `json:"ratingKey"`
	State            string `json:"state"`
	ViewOffset       int64  `json:"viewOffset"`
}

type wsTimelineEntry struct {
	ItemID        string `json:"itemID"`
	Identifier    string `json:"identifier"`
	SectionID     string `json:"sectionID"`
	MetadataState string `json:"metadataState"`
	MediaState    string `json:"mediaState"`
	Type          int    `json:"type"`
	State         int    `json:"state"`
	UpdatedAt     int64  `json:"updatedAt"`
}

func (a *app) listen(ctx context.Context) {
	const (
		minBackoff = time.Second
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := a.connectAndListen(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = minBackoff
		}
		slog.Warn("websocket disconnected, reconnecting",
			"error", err, "backoff", backoff)
		delay := time.NewTimer(backoff)
		select {
		case <-delay.C:
		case <-ctx.Done():
			delay.Stop()
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (a *app) connectAndListen(ctx context.Context) (bool, error) {
	scheme := "ws"
	if a.client.baseURL.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := url.URL{
		Scheme: scheme,
		Host:   a.client.baseURL.Host,
		Path:   "/:/websockets/notifications",
	}

	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Plex-Token": {a.client.token}},
		HTTPClient: a.client.httpClient,
	}

	conn, resp, err := websocket.Dial(ctx, wsURL.String(), opts)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("websocket dial: %w", err)
	}
	defer func() {
		if err := conn.CloseNow(); err != nil {
			slog.Debug("websocket close error", "error", err)
		}
	}()

	slog.Info("websocket connected",
		"server", a.identity.FriendlyName)

	// Limit WebSocket message size to prevent OOM from oversized messages.
	conn.SetReadLimit(1 << 20) // 1 MB

	for {
		_, message, readErr := conn.Read(ctx)
		if readErr != nil {
			return true, fmt.Errorf("websocket read: %w", readErr)
		}
		var notif wsNotification
		if jsonErr := json.Unmarshal(message, &notif); jsonErr != nil {
			slog.Debug("invalid websocket message", "error", jsonErr)
			continue
		}
		a.handleNotification(ctx, &notif)
	}
}

func (a *app) handleNotification(ctx context.Context, notif *wsNotification) {
	switch notif.NotificationContainer.Type {
	case statePlaying:
		if a.cfg.triggerOnPlay {
			a.handlePlaying(ctx, notif.NotificationContainer.PlaySessionStateNotification)
		}
	case "timeline":
		if a.cfg.triggerOnScan {
			a.handleTimeline(ctx, notif.NotificationContainer.TimelineEntry)
		}
	}
}

// isRelevantPlayEvent returns true if a play event should be processed
// (state is playing/paused and has a rating key).
func isRelevantPlayEvent(ev wsPlayEvent) bool {
	if ev.State != statePlaying && ev.State != "paused" {
		return false
	}
	return ev.RatingKey != ""
}

// buildStreamCacheKey builds a deduplication key from user, episode, and
// current stream IDs so we only process when the selection actually changes.
func buildStreamCacheKey(userID, ratingKey string, audioID, subID int) string {
	return fmt.Sprintf("streams:%s:%s:%d:%d", userID, ratingKey, audioID, subID)
}

func (a *app) handlePlaying(ctx context.Context, events []wsPlayEvent) {
	for _, ev := range events {
		if !isRelevantPlayEvent(ev) {
			continue
		}

		// Resolve the user from the session's clientIdentifier.
		userID := a.admin.ID
		username := a.admin.Name
		if ev.ClientIdentifier != "" {
			if uid, uname, err := a.client.getUserFromSession(ctx, ev.ClientIdentifier); err == nil {
				userID = uid
				username = uname
			} else {
				slog.Debug("could not resolve user from session, using admin",
					"client", ev.ClientIdentifier, "error", err)
			}
		}

		// Skip if session state is unchanged (Plex sends repeated notifications).
		sessionCacheKey := "session:" + userID + ":" + ev.SessionKey
		if a.cache.wasRecentlyProcessed(sessionCacheKey) {
			continue
		}

		// Use per-user client to fetch episode (sees that user's stream selections).
		userClient := a.users.clientForUser(userID, a.client)
		episode, err := userClient.getEpisode(ctx, ev.RatingKey)
		if err != nil {
			if !errors.Is(err, errNotFound) {
				slog.Debug("play event: failed to fetch episode",
					"key", ev.RatingKey, "user", username, "error", err)
			}
			continue
		}
		if episode.Type != typeEpisode {
			continue
		}

		// Only trigger when the user's stream selection actually changed.
		// This prevents re-processing on every play progress notification.
		curAudio, curSub := selectedStreams(episode)
		audioID, subID := 0, 0
		if curAudio != nil {
			audioID = curAudio.ID
		}
		if curSub != nil {
			subID = curSub.ID
		}
		streamKey := buildStreamCacheKey(userID, ev.RatingKey, audioID, subID)
		if a.cache.wasRecentlyProcessed(streamKey) {
			continue
		}
		a.cache.markProcessed(streamKey)
		a.cache.markProcessed(sessionCacheKey)

		slog.Info("play event detected",
			"episode", episode.shortName(),
			"user", username,
			"state", ev.State)

		a.changeTracksForEpisode(ctx, userClient, userID, episode, "play")
	}
}

// isRelevantTimelineEntry returns true if a timeline entry should be processed
// (episode type, metadata/media created or updated, non-empty item ID).
func isRelevantTimelineEntry(entry *wsTimelineEntry) bool {
	if entry.Type != plexTypeEpisode {
		return false
	}
	if entry.MetadataState != stateCreated && entry.MetadataState != stateUpdated &&
		entry.MediaState != stateCreated && entry.MediaState != stateUpdated {
		return false
	}
	return entry.ItemID != ""
}

// timelineAction returns "scan_new" if the entry represents a newly created
// item, or "scan_updated" otherwise.
func timelineAction(entry *wsTimelineEntry) string {
	if entry.MetadataState == stateCreated || entry.MediaState == stateCreated {
		return "scan_new"
	}
	return "scan_updated"
}

func (a *app) handleTimeline(ctx context.Context, entries []wsTimelineEntry) {
	for i := range entries {
		entry := &entries[i]
		if !isRelevantTimelineEntry(entry) {
			continue
		}

		cacheKey := "timeline:" + entry.ItemID
		if a.cache.wasRecentlyProcessed(cacheKey) {
			continue
		}

		episode, err := a.client.getEpisode(ctx, entry.ItemID)
		if err != nil {
			slog.Debug("timeline: failed to fetch episode",
				"id", entry.ItemID, "error", err)
			continue
		}
		if episode.Type != typeEpisode {
			continue
		}

		action := timelineAction(entry)

		slog.Info("library scan event detected",
			"episode", episode.shortName(),
			"action", action)

		a.cache.markProcessed(cacheKey)

		// For new/updated episodes, process for ALL users.
		a.processNewOrUpdatedEpisodeAllUsers(ctx, episode, action)
	}
}

// processNewOrUpdatedEpisodeAllUsers processes a new/updated episode for all
// known users (admin + shared).
func (a *app) processNewOrUpdatedEpisodeAllUsers(ctx context.Context, episode *plexEpisode, trigger string) {
	for _, u := range a.users.allUsers(a.client.token) {
		userClient := a.users.clientForUser(u.ID, a.client)
		a.processNewOrUpdatedEpisode(ctx, userClient, u.ID, episode, trigger)
	}
}

// processNewOrUpdatedEpisode handles newly added or updated episodes for a
// specific user. It finds the last watched episode of the show to use as a
// language reference, then applies those language settings to the new episode.
func (a *app) processNewOrUpdatedEpisode(ctx context.Context, userClient *plexClient, userID string, episode *plexEpisode, trigger string) {
	username := a.users.userName(userID)
	showRatingKey := episode.GrandparentRatingKey
	if showRatingKey == "" {
		return
	}

	// Get all episodes of the show using the user's client.
	episodes, err := userClient.getShowEpisodes(ctx, showRatingKey)
	if err != nil {
		slog.Warn("failed to fetch show episodes for reference",
			"show", episode.GrandparentTitle, "user", username, "error", err)
		return
	}

	// Find the last watched episode (highest season+episode number that has
	// audio streams selected) as the reference.
	var reference *plexEpisode
	for i := len(episodes) - 1; i >= 0; i-- {
		ep := &episodes[i]
		if ep.RatingKey == episode.RatingKey {
			continue // Skip the new episode itself.
		}
		full, fetchErr := userClient.getEpisode(ctx, ep.RatingKey)
		if fetchErr != nil {
			continue
		}
		audio, _ := selectedStreams(full)
		if audio != nil {
			reference = full
			break
		}
	}

	if reference == nil {
		// No reference episode found — try language profiles.
		if a.cfg.languageProfiles {
			if a.applyLanguageProfile(ctx, userClient, userID, episode, trigger) {
				return
			}
		}
		slog.Debug("no reference episode found for new episode",
			"episode", episode.shortName(), "user", username)
		return
	}

	// Apply the reference's language settings to just this episode.
	refAudio, refSub := selectedStreams(reference)
	if refAudio == nil {
		return
	}

	changed := a.updateEpisodeStreams(ctx, userClient, episode.RatingKey, refAudio, refSub)
	if changed {
		slog.Info("new/updated episode language set",
			"trigger", trigger,
			"user", username,
			"episode", episode.shortName(),
			"reference", reference.shortName(),
			"audio", streamDesc(refAudio),
			"subtitle", streamDesc(refSub))
	}
}

// findSubtitleByLanguage returns the best subtitle stream matching the given
// language code, preferring higher-quality codecs. Returns nil if none match.
func findSubtitleByLanguage(streams []*plexStream, langCode string) *plexStream {
	var best *plexStream
	bestScore := -1
	for _, s := range streams {
		if s.LanguageCode != langCode {
			continue
		}
		sc := subtitleCodecScore(s.Codec)
		if sc > bestScore {
			best = s
			bestScore = sc
		}
	}
	return best
}

// subtitleCodecScore ranks subtitle codecs by quality/reliability.
// Higher is better: styled text > image-based (source) > plain text (Bazarr).
func subtitleCodecScore(codec string) int {
	switch strings.ToLower(codec) {
	case "ass", "ssa":
		return 3
	case "pgs", "vobsub", "dvdsub", "dvb_subtitle", "hdmv_pgs_subtitle":
		return 2
	case "srt", "subrip", "mov_text", "webvtt":
		return 1
	default:
		return 0
	}
}

// applyLanguageProfile applies a learned language profile to a new episode
// when no reference episode exists in the show. This handles the case where
// a brand new show is added and the user has established preferences
// (e.g., Japanese audio → English subtitles for anime).
func (a *app) applyLanguageProfile(ctx context.Context, userClient *plexClient, userID string, episode *plexEpisode, trigger string) bool {
	username := a.users.userName(userID)
	target, err := userClient.getEpisode(ctx, episode.RatingKey)
	if err != nil {
		return false
	}

	// Get the default audio stream to determine the show's primary language.
	curAudio, curSub := selectedStreams(target)
	if curAudio == nil || curAudio.LanguageCode == "" {
		return false
	}

	subLang, ok := a.cache.getSubtitleLangForAudio(userID, curAudio.LanguageCode)
	if !ok {
		return false
	}

	partID := firstPartID(target)
	if partID == 0 {
		return false
	}

	changed := false

	if subLang == "" {
		// Profile says no subtitles for this audio language.
		if curSub != nil {
			if err := userClient.disableSubtitles(ctx, partID); err != nil {
				slog.Warn("failed to disable subtitles via profile",
					"episode", target.shortName(), "user", username, "error", err)
			} else {
				changed = true
			}
		}
	} else {
		bestSub := findSubtitleByLanguage(subtitleStreams(target), subLang)
		if bestSub != nil && (curSub == nil || curSub.ID != bestSub.ID) {
			if err := userClient.setSubtitleStream(ctx, partID, bestSub.ID); err != nil {
				slog.Warn("failed to set subtitle via profile",
					"episode", target.shortName(), "user", username, "error", err)
			} else {
				changed = true
			}
		}
	}

	if changed {
		slog.Info("language profile applied to new show",
			"trigger", trigger,
			"user", username,
			"episode", target.shortName(),
			"audio_lang", curAudio.LanguageCode,
			"subtitle_lang", subLang)
	}
	return changed
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

func (a *app) runScheduler(ctx context.Context) {
	if !a.cfg.schedulerEnable {
		slog.Info("scheduler disabled")
		return
	}

	slog.Info("scheduler enabled", "time", a.cfg.schedulerTime)

	// Run immediately if never run before or last run was >24h ago.
	a.cache.mu.Lock()
	lastRun := a.cache.data.LastSchedulerRun
	a.cache.mu.Unlock()
	if lastRun == 0 || time.Since(time.Unix(lastRun, 0)) > 24*time.Hour {
		slog.Info("running initial deep analysis")
		a.deepAnalysis(ctx)
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			target := a.parseScheduleTime(now)
			// Check if we're within the minute of the scheduled time.
			if now.Hour() == target.Hour() && now.Minute() == target.Minute() {
				// Only run once per day.
				a.cache.mu.Lock()
				lr := a.cache.data.LastSchedulerRun
				a.cache.mu.Unlock()
				if time.Since(time.Unix(lr, 0)) > 23*time.Hour {
					slog.Info("scheduled deep analysis starting")
					a.deepAnalysis(ctx)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (a *app) parseScheduleTime(now time.Time) time.Time {
	parts := strings.SplitN(a.cfg.schedulerTime, ":", 2)
	hour, minute := 2, 0
	if len(parts) == 2 {
		if h, err := strconv.Atoi(parts[0]); err == nil {
			hour = h
		}
		if m, err := strconv.Atoi(parts[1]); err == nil {
			minute = m
		}
	}
	return time.Date(now.Year(), now.Month(), now.Day(),
		hour, minute, 0, 0, now.Location())
}

func (a *app) deepAnalysis(ctx context.Context) {
	defer func() {
		a.cache.mu.Lock()
		a.cache.data.LastSchedulerRun = time.Now().Unix()
		a.cache.mu.Unlock()
		a.cache.save()
	}()

	sinceUnix := time.Now().Add(-24 * time.Hour).Unix()
	a.processRecentHistory(ctx, sinceUnix)
	a.processRecentlyAdded(ctx, sinceUnix)

	slog.Info("deep analysis completed")
}

// processRecentHistory replays language settings from the last 24h of play history.
func (a *app) processRecentHistory(ctx context.Context, sinceUnix int64) {
	history, err := a.client.getHistory(ctx, sinceUnix)
	if err != nil {
		slog.Warn("scheduler: failed to fetch history", "error", err)
		return
	}
	for _, item := range history {
		if ctx.Err() != nil {
			return
		}
		if item.Type != typeEpisode {
			continue
		}
		if a.shouldIgnoreLibrary(item.LibraryTitle) {
			continue
		}
		userID := item.AccountID.String()
		userClient := a.users.clientForUser(userID, a.client)
		ep, fetchErr := userClient.getEpisode(ctx, item.RatingKey)
		if fetchErr != nil {
			continue
		}
		a.changeTracksForEpisode(ctx, userClient, userID, ep, "scheduler")
	}
}

// processRecentlyAdded applies language settings to recently added episodes for all users.
func (a *app) processRecentlyAdded(ctx context.Context, sinceUnix int64) {
	sections, err := a.client.getShowSections(ctx)
	if err != nil {
		slog.Warn("scheduler: failed to fetch sections", "error", err)
		return
	}

	allUsers := a.users.allUsers(a.client.token)
	for _, section := range sections {
		if ctx.Err() != nil {
			return
		}
		if a.shouldIgnoreLibrary(section.Title) {
			continue
		}
		episodes, err := a.client.getRecentlyAdded(ctx, section.Key, sinceUnix)
		if err != nil {
			slog.Debug("scheduler: failed to fetch recently added",
				"section", section.Title, "error", err)
			continue
		}
		for i := range episodes {
			if ctx.Err() != nil {
				return
			}
			ep := &episodes[i]
			cacheKey := "scheduler:" + ep.RatingKey
			if a.cache.wasRecentlyProcessed(cacheKey) {
				continue
			}
			a.cache.markProcessed(cacheKey)

			slog.Info("scheduler: processing recently added episode",
				"episode", ep.shortName())

			for _, u := range allUsers {
				userClient := a.users.clientForUser(u.ID, a.client)
				full, fetchErr := userClient.getEpisode(ctx, ep.RatingKey)
				if fetchErr != nil {
					continue
				}
				a.processNewOrUpdatedEpisode(ctx, userClient, u.ID, full, "scheduler")
			}
		}
	}

}
