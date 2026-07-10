package hfclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HubModel is the subset of a Hugging Face Hub /api/models record the discovery
// watcher needs. Unknown fields are ignored.
type HubModel struct {
	ID          string   `json:"id"`
	ModelID     string   `json:"modelId"`
	CreatedAt   string   `json:"createdAt"`
	PipelineTag string   `json:"pipeline_tag"`
	Tags        []string `json:"tags"`
	Downloads   int      `json:"downloads"`
	Likes       int      `json:"likes"`
	// Gated is false, "auto", or "manual" depending on endpoint — decode loosely.
	Gated    any `json:"gated"`
	CardData struct {
		License string `json:"license"`
	} `json:"cardData"`
}

// RepoID returns the canonical "org/name" id (the list and detail endpoints
// disagree on which field they populate).
func (m HubModel) RepoID() string {
	if m.ID != "" {
		return m.ID
	}
	return m.ModelID
}

// IsGated reports whether the repo requires access approval.
func (m HubModel) IsGated() bool {
	switch v := m.Gated.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false"
	default:
		return false
	}
}

// License returns the SPDX-ish license id, preferring cardData then a
// "license:<id>" tag. Lower-cased; empty when unknown.
func (m HubModel) License() string {
	if m.CardData.License != "" {
		return strings.ToLower(m.CardData.License)
	}
	for _, t := range m.Tags {
		if rest, ok := strings.CutPrefix(t, "license:"); ok {
			return strings.ToLower(rest)
		}
	}
	return ""
}

// ListModels returns recent models for an author/org, newest first. It retries
// once on HTTP 429 after RetryBackoff (default 2s; set to 0 in tests).
func (c *Client) ListModels(ctx context.Context, author string, limit int) ([]HubModel, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("author", author)
	q.Set("sort", "createdAt")
	q.Set("direction", "-1")
	q.Set("limit", strconv.Itoa(limit))
	q.Set("full", "true")
	u := c.base() + "/api/models?" + q.Encode()

	body, err := c.getWithRetry(ctx, u)
	if err != nil {
		return nil, err
	}
	var models []HubModel
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, fmt.Errorf("hfclient: parse model list for %s: %w", author, err)
	}
	return models, nil
}

// getWithRetry wraps get with a single 429 retry.
func (c *Client) getWithRetry(ctx context.Context, u string) ([]byte, error) {
	body, err := c.get(ctx, u)
	if err == nil {
		return body, nil
	}
	if !strings.Contains(err.Error(), "status 429") {
		return nil, err
	}
	backoff := c.RetryBackoff
	if backoff < 0 {
		return nil, err
	}
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(backoff):
	}
	return c.get(ctx, u)
}
