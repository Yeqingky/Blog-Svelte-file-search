package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultMeiliURL = "https://meilisearch.example.com"
	defaultIndex    = "openlist"
	defaultListen   = "127.0.0.1:8099"
	defaultCORS     = "https://www.example.com,http://localhost:5173"
)

type Config struct {
	MeiliURL    string
	SearchKey   string
	Index       string
	ListenAddr  string
	CORSOrigins map[string]struct{}
	Shares      map[string][]string
}

func loadConfig() (Config, error) {
	values := make(map[string]string)
	file, err := os.Open(".env")
	if err == nil {
		parsed, parseErr := parseEnvFile(file)
		closeErr := file.Close()
		if parseErr != nil {
			return Config{}, parseErr
		}
		if closeErr != nil {
			return Config{}, closeErr
		}
		for key, value := range parsed {
			values[key] = value
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("open .env: %w", err)
	}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return parseConfig(values)
}

func parseEnvFile(reader io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf(".env line %d: expected KEY=VALUE", lineNumber)
		}
		key = strings.TrimSpace(key)
		if !validEnvName(key) {
			return nil, fmt.Errorf(".env line %d: invalid variable name", lineNumber)
		}
		value = strings.TrimSpace(value)
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			quote := value[0]
			if len(value) < 2 || value[len(value)-1] != quote {
				return nil, fmt.Errorf(".env line %d: unclosed quoted value", lineNumber)
			}
			value = value[1 : len(value)-1]
		} else if len(value) > 0 && (value[len(value)-1] == '\'' || value[len(value)-1] == '"') {
			return nil, fmt.Errorf(".env line %d: unmatched quote", lineNumber)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read .env: %w", err)
	}
	return values, nil
}

func parseConfig(values map[string]string) (Config, error) {
	meiliURL := strings.TrimSpace(values["MEILI_URL"])
	if meiliURL == "" {
		meiliURL = defaultMeiliURL
	}
	meiliURL, err := normalizeMeiliURL(meiliURL)
	if err != nil {
		return Config{}, err
	}

	searchKey := strings.TrimSpace(values["MEILI_SEARCH_KEY"])
	if searchKey == "" {
		return Config{}, fmt.Errorf("MEILI_SEARCH_KEY is required")
	}

	index := strings.TrimSpace(values["MEILI_INDEX"])
	if index == "" {
		index = defaultIndex
	}
	if !validIndexID(index) {
		return Config{}, fmt.Errorf("MEILI_INDEX must contain only letters, digits, hyphens, or underscores")
	}

	listenAddr := strings.TrimSpace(values["LISTEN_ADDR"])
	if listenAddr == "" {
		listenAddr = defaultListen
	}
	if err := validateListenAddr(listenAddr); err != nil {
		return Config{}, err
	}

	corsValue, hasCORS := values["CORS_ORIGINS"]
	if !hasCORS {
		corsValue = defaultCORS
	}
	corsOrigins, err := parseCORSOrigins(corsValue)
	if err != nil {
		return Config{}, err
	}

	shares, err := parseShares(values)
	if err != nil {
		return Config{}, err
	}
	if len(shares) == 0 {
		return Config{}, fmt.Errorf("at least one SHARE_<sid> mapping is required")
	}

	return Config{
		MeiliURL:    meiliURL,
		SearchKey:   searchKey,
		Index:       index,
		ListenAddr:  listenAddr,
		CORSOrigins: corsOrigins,
		Shares:      shares,
	}, nil
}

func normalizeMeiliURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("MEILI_URL must be an http or https base URL without credentials, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func parseCORSOrigins(raw string) (map[string]struct{}, error) {
	origins := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		origin, err := normalizeOrigin(item)
		if err != nil {
			return nil, fmt.Errorf("invalid CORS origin %q", item)
		}
		origins[origin] = struct{}{}
	}
	return origins, nil
}

func normalizeOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid origin")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func parseShares(values map[string]string) (map[string][]string, error) {
	keys := make([]string, 0)
	for key := range values {
		if strings.HasPrefix(key, "SHARE_") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	shares := make(map[string][]string, len(keys))
	for _, key := range keys {
		sid, err := normalizeShareID(strings.TrimPrefix(key, "SHARE_"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if _, exists := shares[sid]; exists {
			return nil, fmt.Errorf("duplicate share mapping for %q", sid)
		}
		paths, err := parseSharePaths(values[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		shares[sid] = paths
	}
	return shares, nil
}

func parseSharePaths(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("share path is required")
	}
	paths := []string{raw}
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &paths); err != nil {
			return nil, fmt.Errorf("invalid JSON path list: %w", err)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one share path is required")
	}

	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, item := range paths {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("share paths must not be empty")
		}
		if !strings.HasPrefix(item, "/") {
			item = "/" + item
		}
		item = path.Clean(item)
		if _, exists := seen[item]; !exists {
			seen[item] = struct{}{}
			normalized = append(normalized, item)
		}
	}
	return normalized, nil
}

func normalizeShareID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "/@s/") {
		value = strings.TrimPrefix(value, "/@s/")
	}
	if value == "" {
		return "", fmt.Errorf("share id is required")
	}
	for i := range len(value) {
		ch := value[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			return "", fmt.Errorf("invalid share id")
		}
	}
	return value, nil
}

func validEnvName(value string) bool {
	if value == "" || !((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z') || value[0] == '_') {
		return false
	}
	for i := 1; i < len(value); i++ {
		ch := value[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_') {
			return false
		}
	}
	return true
}

func validIndexID(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func validateListenAddr(value string) error {
	_, portText, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR must be host:port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("LISTEN_ADDR port must be between 0 and 65535")
	}
	return nil
}
