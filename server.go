package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type API struct {
	cfg     Config
	client  *http.Client
	logger  *log.Logger
	limiter *rateLimiter
	roots   map[string][]shareRoot
}

type shareRoot struct {
	path       string
	hash       string
	parentHash string
}

type searchHit struct {
	Share string `json:"share"`
	Path  string `json:"path"`
	Dir   string `json:"dir"`
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

type searchResponse struct {
	Share string      `json:"share,omitempty"`
	Query string      `json:"query"`
	Total int64       `json:"total"`
	Hits  []searchHit `json:"hits"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func NewAPI(cfg Config, client *http.Client, logger *log.Logger) *API {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	if logger == nil {
		logger = log.Default()
	}
	roots := make(map[string][]shareRoot, len(cfg.Shares))
	for sid, configuredRoots := range cfg.Shares {
		shareRoots := make([]shareRoot, 0, len(configuredRoots))
		for _, root := range configuredRoots {
			root = path.Clean("/" + strings.TrimPrefix(root, "/"))
			shareRoots = append(shareRoots, shareRoot{path: root, hash: hashPath(root), parentHash: hashPath(path.Dir(root))})
		}
		roots[sid] = shareRoots
	}
	return &API{
		cfg:     cfg,
		client:  client,
		logger:  logger,
		limiter: newRateLimiter(30.0/60.0, 10),
		roots:   roots,
	}
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (a *API) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	clientIP := requestIP(request)
	responseWriter := &statusWriter{ResponseWriter: writer}
	defer func() {
		if responseWriter.status == 0 {
			responseWriter.status = http.StatusOK
		}
		a.logger.Printf("time=%s ip=%q share=%q q=%q status=%d duration=%s", started.Format(time.RFC3339Nano), clientIP, request.URL.Query().Get("share"), request.URL.Query().Get("q"), responseWriter.status, time.Since(started).Round(time.Millisecond))
	}()

	originAllowed := a.applyCORS(responseWriter, request)
	if request.Method == http.MethodOptions {
		a.handleOptions(responseWriter, request, originAllowed)
		return
	}

	switch request.URL.Path {
	case "/search":
		if request.Method != http.MethodGet {
			responseWriter.Header().Set("Allow", "GET, OPTIONS")
			writeError(responseWriter, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleSearch(responseWriter, request, clientIP)
	case "/healthz":
		if request.Method != http.MethodGet {
			responseWriter.Header().Set("Allow", "GET, OPTIONS")
			writeError(responseWriter, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleHealth(responseWriter, request)
	default:
		writeError(responseWriter, http.StatusNotFound, "not found")
	}
}

func (a *API) applyCORS(writer http.ResponseWriter, request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return false
	}
	writer.Header().Add("Vary", "Origin")
	canonical, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	if _, ok := a.cfg.CORSOrigins[canonical]; !ok {
		return false
	}
	writer.Header().Set("Access-Control-Allow-Origin", origin)
	return true
}

func (a *API) handleOptions(writer http.ResponseWriter, request *http.Request, originAllowed bool) {
	if request.URL.Path != "/search" && request.URL.Path != "/healthz" {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	if request.Header.Get("Origin") != "" && !originAllowed {
		writeError(writer, http.StatusForbidden, "origin not allowed")
		return
	}
	writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	writer.Header().Set("Access-Control-Max-Age", "600")
	writer.WriteHeader(http.StatusNoContent)
}

func (a *API) handleSearch(writer http.ResponseWriter, request *http.Request, clientIP string) {
	if !a.limiter.allow(clientIP, time.Now()) {
		writeError(writer, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	values := request.URL.Query()
	rawShare := strings.TrimSpace(values.Get("share"))
	share := ""
	shareIDs := make([]string, 0, len(a.roots))
	if rawShare != "" {
		normalizedShare, err := normalizeShareID(rawShare)
		if err != nil {
			writeError(writer, http.StatusNotFound, "share not found")
			return
		}
		if _, ok := a.roots[normalizedShare]; !ok {
			writeError(writer, http.StatusNotFound, "share not found")
			return
		}
		share = normalizedShare
		shareIDs = append(shareIDs, normalizedShare)
	} else {
		for shareID := range a.roots {
			shareIDs = append(shareIDs, shareID)
		}
		sort.Strings(shareIDs)
	}

	query := strings.TrimSpace(values.Get("q"))
	queryLength := utf8.RuneCountInString(query)
	if queryLength == 0 || queryLength > 128 {
		writeError(writer, http.StatusBadRequest, "q must contain 1 to 128 characters")
		return
	}

	dir, err := cleanShareDir(values.Get("dir"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "dir must stay within the share")
		return
	}

	hashes := make([]string, 0, len(shareIDs))
	for _, shareID := range shareIDs {
		for _, root := range a.roots[shareID] {
			if dir == "" {
				hashes = append(hashes, root.hash)
				continue
			}
			hashes = append(hashes, hashPath(path.Join(root.path, dir)))
		}
	}
	filter := formatFilter("parent_path_hashes", hashes)
	result, err := a.searchMeili(request.Context(), query, filter, maxMeiliSearchHits)
	if err != nil {
		a.logger.Printf("WARNING share=%q Meilisearch request failed: %v", share, err)
		writeError(writer, http.StatusBadGateway, "search service unavailable")
		return
	}

	hits := make([]searchHit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		for _, shareID := range shareIDs {
			visiblePath, matched := clipHitPath(hit, a.roots[shareID])
			if !matched {
				continue
			}
			dirName := path.Dir(visiblePath)
			if dirName == "." {
				dirName = ""
			}
			hits = append(hits, searchHit{
				Share: shareID,
				Path:  visiblePath,
				Dir:   dirName,
				Name:  hit.Name,
				IsDir: hit.IsDir,
				Size:  hit.Size,
			})
			break
		}
	}

	writeJSON(writer, http.StatusOK, searchResponse{
		Share: share,
		Query: query,
		Total: result.total(),
		Hits:  hits,
	})
}

func (a *API) handleHealth(writer http.ResponseWriter, request *http.Request) {
	probe, err := http.NewRequestWithContext(request.Context(), http.MethodGet, a.endpoint("/health"), nil)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "search service unavailable")
		return
	}
	response, err := a.client.Do(probe)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "search service unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		writeError(writer, http.StatusServiceUnavailable, "search service unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		writeError(writer, http.StatusServiceUnavailable, "search service unavailable")
		return
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil || health.Status != "available" {
		writeError(writer, http.StatusServiceUnavailable, "search service unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "available"})
}

func cleanShareDir(value string) (string, error) {
	if strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("absolute or invalid directory")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("directory traversal")
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("directory traversal")
	}
	return cleaned, nil
}

func clipHitPath(hit meiliHit, roots []shareRoot) (string, bool) {
	if hit.Name == "" || hit.Name == "." || hit.Name == ".." || strings.Contains(hit.Name, "/") {
		return "", false
	}
	parent := strings.TrimSpace(hit.Parent)
	if parent == "" {
		parent = "/"
	} else if !strings.HasPrefix(parent, "/") {
		parent = "/" + parent
	}
	for _, segment := range strings.Split(parent, "/") {
		if segment == ".." {
			return "", false
		}
	}
	fullPath := path.Join(path.Clean(parent), hit.Name)
	return relativeSharePath(fullPath, roots)
}

func relativeSharePath(fullPath string, roots []shareRoot) (string, bool) {
	fullPath = path.Clean(fullPath)
	var bestRoot shareRoot
	for _, root := range roots {
		rootPath := root.path
		matches := fullPath == rootPath || (rootPath == "/" && strings.HasPrefix(fullPath, "/")) || strings.HasPrefix(fullPath, rootPath+"/")
		if matches && len(rootPath) > len(bestRoot.path) {
			bestRoot = root
		}
	}
	if bestRoot.path == "" {
		return "", false
	}

	remainder := ""
	if bestRoot.path == "/" {
		remainder = strings.TrimPrefix(fullPath, "/")
	} else if fullPath != bestRoot.path {
		remainder = strings.TrimPrefix(fullPath, bestRoot.path+"/")
	}
	return remainder, remainder != ""
}

func requestIP(request *http.Request) string {
	remote := request.RemoteAddr
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	remoteIP := net.ParseIP(host)
	if remoteIP != nil && remoteIP.IsLoopback() {
		forwarded := net.ParseIP(strings.TrimSpace(request.Header.Get("X-Real-IP")))
		if forwarded != nil {
			return forwarded.String()
		}
	}
	if remoteIP == nil {
		return remote
	}
	return remoteIP.String()
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, errorResponse{Error: message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
