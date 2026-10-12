package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WebResult 是一条联网检索结果。
type WebResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// ErrSearchDisabled 表示没有配置联网检索服务，需要降级处理。
var ErrSearchDisabled = errors.New("未配置联网检索服务（SEARCH_PROVIDER / SEARCH_API_KEY）")

// WebSearch 按配置调用外部搜索服务。未配置或调用失败时调用方应降级处理。
func WebSearch(ctx context.Context, cfg *Config, query string) ([]WebResult, error) {
	if !cfg.SearchEnabled() {
		return nil, ErrSearchDisabled
	}
	switch strings.ToLower(cfg.SearchProvider) {
	case "tavily":
		return searchTavily(ctx, cfg, query)
	case "bocha":
		return searchBocha(ctx, cfg, query)
	default:
		return nil, fmt.Errorf("不支持的 SEARCH_PROVIDER: %s（可用 tavily / bocha）", cfg.SearchProvider)
	}
}

func searchTavily(ctx context.Context, cfg *Config, query string) ([]WebResult, error) {
	body, _ := json.Marshal(map[string]any{
		"api_key":      cfg.SearchAPIKey,
		"query":        query,
		"max_results":  cfg.SearchTopK,
		"search_depth": "basic",
	})
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := postJSON(ctx, "https://api.tavily.com/search", nil, body, &out); err != nil {
		return nil, err
	}
	res := make([]WebResult, 0, len(out.Results))
	for _, r := range out.Results {
		res = append(res, WebResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return res, nil
}

func searchBocha(ctx context.Context, cfg *Config, query string) ([]WebResult, error) {
	body, _ := json.Marshal(map[string]any{
		"query":   query,
		"count":   cfg.SearchTopK,
		"summary": true,
	})
	var out struct {
		Data struct {
			WebPages struct {
				Value []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Snippet string `json:"snippet"`
					Summary string `json:"summary"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	header := map[string]string{"Authorization": "Bearer " + cfg.SearchAPIKey}
	if err := postJSON(ctx, "https://api.bochaai.com/v1/web-search", header, body, &out); err != nil {
		return nil, err
	}
	res := make([]WebResult, 0, len(out.Data.WebPages.Value))
	for _, r := range out.Data.WebPages.Value {
		snippet := r.Summary
		if snippet == "" {
			snippet = r.Snippet
		}
		res = append(res, WebResult{Title: r.Name, URL: r.URL, Snippet: snippet})
	}
	return res, nil
}

func postJSON(ctx context.Context, url string, header map[string]string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("联网检索请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("联网检索返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))[:min(200, len(raw))])
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析检索结果失败: %w", err)
	}
	return nil
}
