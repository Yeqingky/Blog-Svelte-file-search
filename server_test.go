package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseEnvFileAndConfig(t *testing.T) {
	values, err := parseEnvFile(strings.NewReader(`# sample
export MEILI_URL=https://file.example.com/
MEILI_SEARCH_KEY="fixture-only"
SHARE_demo=["relative/a", "/other/../normalized"]
`))
	if err != nil {
		t.Fatal(err)
	}
	values["MEILI_URL"] = "https://override.example.com"

	cfg, err := parseConfig(values)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeiliURL != "https://override.example.com" {
		t.Fatalf("environment value did not override .env: %q", cfg.MeiliURL)
	}
	if !reflect.DeepEqual(cfg.Shares["demo"], []string{"/relative/a", "/normalized"}) {
		t.Fatalf("unexpected normalized share paths: %#v", cfg.Shares["demo"])
	}
	if _, ok := cfg.CORSOrigins["https://www.example.com"]; !ok {
		t.Fatal("default CORS origin was not configured")
	}
}

func TestDefaultMeiliURLUsesExampleHost(t *testing.T) {
	cfg, err := parseConfig(map[string]string{
		"MEILI_SEARCH_KEY": "fixture-only",
		"SHARE_demo":       "/example/share",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeiliURL != "https://meilisearch.example.com" {
		t.Fatalf("unexpected default Meilisearch URL: %q", cfg.MeiliURL)
	}
}

func TestSearchScopesAndClipsResults(t *testing.T) {
	var received meiliSearchRequest
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/indexes/openlist/search" {
			t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer fixture-only" {
			t.Errorf("upstream search authorization was not sent")
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"totalHits":2,"hits":[{"parent":"/example/share/folder/sub","name":"match.txt","is_dir":false,"size":123},{"parent":"/other/alternate/folder/sub","name":"second.txt","is_dir":true,"size":0},{"parent":"/example/share-escape/folder/sub","name":"leak.txt","is_dir":false,"size":999}]}`)
	}))
	defer backend.Close()

	cfg := testConfig(backend.URL)
	cfg.Shares["demo"] = []string{"/example/share", "/other/alternate"}
	api := NewAPI(cfg, backend.Client(), log.New(io.Discard, "", 0))
	query := url.Values{
		"share": {"/@s/demo"},
		"q":     {"  keyword  "},
		"dir":   {"folder/sub"},
	}
	request := httptest.NewRequest(http.MethodGet, "/search?"+query.Encode(), nil)
	request.Header.Set("Origin", "https://www.example.com")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://www.example.com" {
		t.Fatal("allowed origin was not returned")
	}
	if received.Query != "keyword" || received.Limit != maxMeiliSearchHits {
		t.Fatalf("unexpected upstream query or result limit: %#v", received)
	}
	if received.Filter != "(parent_path_hashes = '052b493fa2357ae5913112c1eb557688b10e2a65' OR parent_path_hashes = 'f17f19b6a4adddd50a84101bace5b87eb08638b9')" {
		t.Fatalf("unexpected scope filter: %q", received.Filter)
	}
	if !reflect.DeepEqual(received.AttributesToRetrieve, []string{"parent", "name", "is_dir", "size"}) {
		t.Fatalf("unexpected retrieved attributes: %#v", received.AttributesToRetrieve)
	}

	var result searchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Share != "demo" || result.Query != "keyword" || result.Total != 2 {
		t.Fatalf("unexpected response metadata: %#v", result)
	}
	var responseFields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &responseFields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"page", "perPage"} {
		if _, ok := responseFields[field]; ok {
			t.Errorf("response includes obsolete field %q", field)
		}
	}
	if len(result.Hits) != 2 {
		t.Fatalf("expected two in-scope results, got %#v", result.Hits)
	}
	if result.Hits[0].Share != "demo" || result.Hits[0].Path != "folder/sub/match.txt" || result.Hits[0].Dir != "folder/sub" || result.Hits[0].Name != "match.txt" || result.Hits[0].Size != 123 {
		t.Fatalf("unexpected first result: %#v", result.Hits[0])
	}
	if result.Hits[1].Share != "demo" || result.Hits[1].Path != "folder/sub/second.txt" || !result.Hits[1].IsDir {
		t.Fatalf("unexpected second result: %#v", result.Hits[1])
	}
	if strings.Contains(response.Body.String(), "/example/") || strings.Contains(response.Body.String(), "/other/") || strings.Contains(response.Body.String(), "leak.txt") {
		t.Fatal("response exposed a physical path or an out-of-scope hit")
	}
}

func TestSearchReturnsAllHitsInSingleQuery(t *testing.T) {
	const resultCount = 75
	var calls atomic.Int32
	var received meiliSearchRequest
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if received.Limit != maxMeiliSearchHits {
			t.Errorf("expected upstream limit %d, got %d", maxMeiliSearchHits, received.Limit)
		}

		hits := make([]map[string]any, resultCount)
		for index := range hits {
			hits[index] = map[string]any{
				"parent": "/example/share",
				"name":   fmt.Sprintf("file-%02d.txt", index),
				"is_dir": false,
				"size":   index,
			}
		}
		if err := json.NewEncoder(writer).Encode(struct {
			TotalHits int              `json:"totalHits"`
			Hits      []map[string]any `json:"hits"`
		}{TotalHits: resultCount, Hits: hits}); err != nil {
			t.Errorf("encode upstream response: %v", err)
		}
	}))
	defer backend.Close()
	api := NewAPI(testConfig(backend.URL), backend.Client(), log.New(io.Discard, "", 0))

	request := httptest.NewRequest(http.MethodGet, "/search?share=demo&q=term", nil)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d: %s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one upstream query, got %d", calls.Load())
	}

	var result searchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != resultCount || len(result.Hits) != resultCount {
		t.Fatalf("expected all %d hits in one response, got total=%d hits=%d", resultCount, result.Total, len(result.Hits))
	}
	if result.Hits[0].Path != "file-00.txt" || result.Hits[resultCount-1].Path != "file-74.txt" {
		t.Fatalf("unexpected first or last hit: first=%#v last=%#v", result.Hits[0], result.Hits[resultCount-1])
	}
}

func TestSearchWithoutShareIncludesShareAndShareRelativePaths(t *testing.T) {
	var received meiliSearchRequest
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		_, _ = fmt.Fprint(writer, `{"totalHits":2,"hits":[{"parent":"/example/public-root/mc","name":"a.txt","is_dir":false,"size":10},{"parent":"/other/public-root/mc","name":"b.txt","is_dir":false,"size":20}]}`)
	}))
	defer backend.Close()

	cfg := testConfig(backend.URL)
	cfg.Shares = map[string][]string{
		"alpha": {"/example/public-root"},
		"beta":  {"/other/public-root"},
	}
	api := NewAPI(cfg, backend.Client(), log.New(io.Discard, "", 0))

	query := url.Values{"q": {"example-query"}}
	request := httptest.NewRequest(http.MethodGet, "/search?"+query.Encode(), nil)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d: %s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one cross-share upstream query, got %d", calls.Load())
	}
	if received.Query != "example-query" {
		t.Fatalf("unexpected upstream query: %q", received.Query)
	}
	wantFilter := "(parent_path_hashes = '" + hashPath("/example/public-root") + "' OR parent_path_hashes = '" + hashPath("/other/public-root") + "')"
	if received.Filter != wantFilter {
		t.Fatalf("unexpected cross-share filter: %q", received.Filter)
	}

	var result searchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["share"]; ok {
		t.Fatal("q-only response must identify share on each hit, not at top level")
	}
	if result.Total != 2 || len(result.Hits) != 2 {
		t.Fatalf("unexpected multi-share result count: total=%d hits=%#v", result.Total, result.Hits)
	}
	if result.Hits[0].Share != "alpha" || result.Hits[0].Path != "mc/a.txt" || result.Hits[0].Dir != "mc" {
		t.Fatalf("unexpected alpha hit: %#v", result.Hits[0])
	}
	if result.Hits[1].Share != "beta" || result.Hits[1].Path != "mc/b.txt" || result.Hits[1].Dir != "mc" {
		t.Fatalf("unexpected beta hit: %#v", result.Hits[1])
	}
	if strings.Contains(response.Body.String(), "public-root/") {
		t.Fatal("share root prefix leaked into the returned paths")
	}
}

func TestAllowedOriginPreflight(t *testing.T) {
	api := NewAPI(testConfig("http://search.example.com"), nil, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodOptions, "/search", nil)
	request.Header.Set("Origin", "https://www.example.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("expected successful preflight, got %d", response.Code)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://www.example.com" || response.Header().Get("Access-Control-Allow-Methods") != "GET, OPTIONS" {
		t.Fatalf("preflight omitted allowed origin or method: %#v", response.Header())
	}
}

func TestSearchRejectsTraversalBeforeUpstreamRequest(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(writer, `{"totalHits":0,"hits":[]}`)
	}))
	defer backend.Close()
	api := NewAPI(testConfig(backend.URL), backend.Client(), log.New(io.Discard, "", 0))

	request := httptest.NewRequest(http.MethodGet, "/search?share=demo&q=term&dir=..%2Foutside", nil)
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected traversal to be rejected, got %d", response.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid directory reached Meilisearch %d times", calls.Load())
	}
}

func TestSearchInputBoundaries(t *testing.T) {
	var calls atomic.Int32
	var received meiliSearchRequest
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode search request: %v", err)
		}
		_, _ = fmt.Fprint(writer, `{"totalHits":0,"hits":[]}`)
	}))
	defer backend.Close()
	api := NewAPI(testConfig(backend.URL), backend.Client(), log.New(io.Discard, "", 0))

	maxQuery := strings.Repeat("界", 128)
	validQuery := url.Values{"share": {"demo"}, "q": {maxQuery}}
	validRequest := httptest.NewRequest(http.MethodGet, "/search?"+validQuery.Encode(), nil)
	validResponse := httptest.NewRecorder()
	api.ServeHTTP(validResponse, validRequest)
	if validResponse.Code != http.StatusOK || received.Query != maxQuery {
		t.Fatalf("128-character query should be accepted and forwarded: status=%d query=%q", validResponse.Code, received.Query)
	}

	cases := []struct {
		name   string
		query  url.Values
		status int
	}{
		{name: "query too long", query: url.Values{"share": {"demo"}, "q": {strings.Repeat("界", 129)}}, status: http.StatusBadRequest},
		{name: "missing query", query: url.Values{"share": {"demo"}}, status: http.StatusBadRequest},
		{name: "unknown share", query: url.Values{"share": {"unknown"}, "q": {"term"}}, status: http.StatusNotFound},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/search?"+test.query.Encode(), nil)
			response := httptest.NewRecorder()
			api.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("expected status %d, got %d: %s", test.status, response.Code, response.Body.String())
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid requests reached Meilisearch: %d upstream calls", calls.Load()-1)
	}
}

func TestHealthzProbesMeilisearchWithoutSearchKey(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/health" {
			t.Errorf("unexpected health probe: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Error("health probe unexpectedly sent an authorization header")
		}
		_, _ = fmt.Fprint(writer, `{"status":"available"}`)
	}))
	defer backend.Close()
	api := NewAPI(testConfig(backend.URL), backend.Client(), log.New(io.Discard, "", 0))

	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"available"}` {
		t.Fatalf("unexpected health response: %d %s", response.Code, response.Body.String())
	}
}

func TestStartupCheckWarnsForFileRoot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var search meiliSearchRequest
		if err := json.NewDecoder(request.Body).Decode(&search); err != nil {
			t.Errorf("decode startup check: %v", err)
			return
		}
		if search.Query == "" {
			_, _ = fmt.Fprint(writer, `{"totalHits":0,"hits":[]}`)
			return
		}
		if search.Query != "document.pdf" {
			t.Errorf("unexpected file lookup query: %q", search.Query)
		}
		_, _ = fmt.Fprint(writer, `{"totalHits":1,"hits":[{"parent":"/example","name":"document.pdf","is_dir":false,"size":20}]}`)
	}))
	defer backend.Close()

	cfg := testConfig(backend.URL)
	cfg.Shares["demo"] = []string{"/example/document.pdf"}
	var logs bytes.Buffer
	api := NewAPI(cfg, backend.Client(), log.New(&logs, "", 0))
	api.CheckShares(context.Background())

	if !strings.Contains(logs.String(), "points to a file") {
		t.Fatalf("expected a warning for a file share root, got %q", logs.String())
	}
}

func TestSearchRateLimitReturns429AfterBurst(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(writer, `{"totalHits":0,"hits":[]}`)
	}))
	defer backend.Close()
	api := NewAPI(testConfig(backend.URL), backend.Client(), log.New(io.Discard, "", 0))

	statuses := make([]int, 0, 11)
	for range 11 {
		request := httptest.NewRequest(http.MethodGet, "/search?share=demo&q=term", nil)
		request.RemoteAddr = "203.0.113.10:1234"
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		statuses = append(statuses, response.Code)
	}
	if statuses[9] != http.StatusOK || statuses[10] != http.StatusTooManyRequests {
		t.Fatalf("expected ten requests then rate limit, got %v", statuses)
	}
	if calls.Load() != 10 {
		t.Fatalf("rate-limited request reached Meilisearch: %d upstream calls", calls.Load())
	}
}

func testConfig(meiliURL string) Config {
	return Config{
		MeiliURL:    meiliURL,
		SearchKey:   "fixture-only",
		Index:       "openlist",
		ListenAddr:  "127.0.0.1:8099",
		CORSOrigins: map[string]struct{}{"https://www.example.com": {}},
		Shares:      map[string][]string{"demo": {"/example/share"}},
	}
}
