// Package importer migrates the pre-Postgres data into the new schema: member
// journals and trades from Notion, gamification state from the BadgerDB volume.
//
// It exists only for the cutover. The application itself never imports this
// package, which is why the BadgerDB dependency appears in go.mod alongside pgx:
// the migration must read the old store once, and deleting the adapter did not
// delete the data.
package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// NotionAPI is the version this migration was written against. It matches the
// pinned version the deleted Notion adapter used.
const NotionAPI = "2022-06-28"

// NotionClient reads the subset of the Notion API the migration needs.
type NotionClient struct {
	token   string
	baseURL string
	client  *http.Client
}

// NewNotionClient builds a client for the given integration token.
func NewNotionClient(token string) *NotionClient {
	return &NotionClient{
		token:   token,
		baseURL: "https://api.notion.com/v1",
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *NotionClient) call(ctx context.Context, method, path string, body any, out any) error {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", NotionAPI)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, truncate(buf.String(), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(buf.Bytes(), out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// BlockChildren lists every child block of a block or page, following the
// pagination cursor. The migration must see all of them: stopping at the first
// page would silently drop members or trades.
func (c *NotionClient) BlockChildren(ctx context.Context, blockID string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	cursor := ""
	for {
		path := "/blocks/" + notionID(blockID) + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + cursor
		}
		var page struct {
			Results    []json.RawMessage `json:"results"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if !page.HasMore || page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// QueryDatabase returns every page in a database.
func (c *NotionClient) QueryDatabase(ctx context.Context, databaseID string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	cursor := ""
	for {
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		var page struct {
			Results    []json.RawMessage `json:"results"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := c.call(ctx, http.MethodPost, "/databases/"+notionID(databaseID)+"/query", body, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if !page.HasMore || page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// notionID strips the dashes Notion accepts but URL paths do not need.
func notionID(id string) string {
	return strings.ReplaceAll(strings.TrimSpace(id), "-", "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
