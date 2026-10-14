package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

const (
	maxMeiliResponseBytes = 2 << 20
	maxMeiliSearchHits    = 1000
)

var retrievedAttributes = []string{"parent", "name", "is_dir", "size"}

type meiliSearchRequest struct {
	Query                string   `json:"q"`
	Filter               string   `json:"filter"`
	Limit                int      `json:"limit"`
	AttributesToRetrieve []string `json:"attributesToRetrieve"`
}

type meiliHit struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Size   int64  `json:"size"`
}

type meiliSearchResponse struct {
	Hits               []meiliHit `json:"hits"`
	TotalHits          *int64     `json:"totalHits"`
	EstimatedTotalHits *int64     `json:"estimatedTotalHits"`
	NbHits             *int64     `json:"nbHits"`
}

func (response meiliSearchResponse) total() int64 {
	for _, value := range []*int64{response.TotalHits, response.EstimatedTotalHits, response.NbHits} {
		if value != nil {
			if *value < 0 {
				return 0
			}
			return *value
		}
	}
	return 0
}

func (a *API) searchMeili(ctx context.Context, query, filter string, limit int) (meiliSearchResponse, error) {
	requestBody := meiliSearchRequest{
		Query:                query,
		Filter:               filter,
		Limit:                limit,
		AttributesToRetrieve: retrievedAttributes,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return meiliSearchResponse{}, fmt.Errorf("encode search request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint("/indexes/"+a.cfg.Index+"/search"), bytes.NewReader(body))
	if err != nil {
		return meiliSearchResponse{}, fmt.Errorf("create search request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+a.cfg.SearchKey)
	request.Header.Set("Content-Type", "application/json")

	response, err := a.client.Do(request)
	if err != nil {
		return meiliSearchResponse{}, fmt.Errorf("send search request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return meiliSearchResponse{}, fmt.Errorf("Meilisearch returned HTTP %d", response.StatusCode)
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxMeiliResponseBytes+1))
	if err != nil {
		return meiliSearchResponse{}, fmt.Errorf("read search response: %w", err)
	}
	if len(responseBody) > maxMeiliResponseBytes {
		return meiliSearchResponse{}, fmt.Errorf("search response exceeds size limit")
	}
	var result meiliSearchResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return meiliSearchResponse{}, fmt.Errorf("decode search response: %w", err)
	}
	return result, nil
}

func (a *API) endpoint(suffix string) string {
	base, _ := url.Parse(a.cfg.MeiliURL)
	base.Path = strings.TrimRight(base.Path, "/") + suffix
	base.RawPath = ""
	return base.String()
}

func hashPath(value string) string {
	sum := sha1.Sum([]byte(path.Clean(value)))
	return hex.EncodeToString(sum[:])
}

func formatFilter(field string, hashes []string) string {
	parts := make([]string, 0, len(hashes))
	for _, value := range hashes {
		parts = append(parts, field+" = '"+value+"'")
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func (a *API) CheckShares(ctx context.Context) {
	shareIDs := make([]string, 0, len(a.roots))
	for sid := range a.roots {
		shareIDs = append(shareIDs, sid)
	}
	sort.Strings(shareIDs)
	for _, sid := range shareIDs {
		for _, root := range a.roots[sid] {
			filter := formatFilter("parent_hash", []string{root.hash})
			result, err := a.searchMeili(ctx, "", filter, 1)
			if err != nil {
				a.logger.Printf("WARNING share=%q root=%q index check failed: %v", sid, root.path, err)
				continue
			}
			if result.total() == 0 {
				if a.shareRootIsFile(ctx, root) {
					a.logger.Printf("WARNING share=%q root=%q points to a file; share roots should be directories", sid, root.path)
					continue
				}
				a.logger.Printf("WARNING share=%q root=%q has no indexed direct children; the path may be incorrect or indexing may still be in progress", sid, root.path)
				continue
			}
			a.logger.Printf("INFO share=%q root=%q indexed_direct_children=%d", sid, root.path, result.total())
		}
	}
}

func (a *API) shareRootIsFile(ctx context.Context, root shareRoot) bool {
	filter := formatFilter("parent_hash", []string{root.parentHash})
	result, err := a.searchMeili(ctx, path.Base(root.path), filter, 50)
	if err != nil {
		return false
	}
	expectedName := path.Base(root.path)
	for _, hit := range result.Hits {
		if hit.Name != expectedName || hit.IsDir {
			continue
		}
		parent := strings.TrimSpace(hit.Parent)
		if parent == "" {
			parent = "/"
		} else if !strings.HasPrefix(parent, "/") {
			parent = "/" + parent
		}
		if path.Join(path.Clean(parent), hit.Name) == root.path {
			return true
		}
	}
	return false
}
