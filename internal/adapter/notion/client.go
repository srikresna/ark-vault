package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"time"
)

const (
	baseURL       = "https://api.notion.com/v1"
	notionVersion = "2022-06-28"

	requestTimeout = 30 * time.Second

	// maxAttempts bounds retries for transient Notion failures (429/5xx and
	// network errors). 1 initial attempt + 3 retries.
	maxAttempts = 4

	// maxRetryDelay caps how long a single retry can be delayed, so a hostile
	// or misconfigured Retry-After header cannot stall the bot indefinitely.
	maxRetryDelay = 10 * time.Second

	// pageSize is Notion's maximum page size; smaller pages would silently
	// multiply request counts.
	pageSize = 100

	// maxPages bounds pagination. Notion has no server-side cap on result sets,
	// so this is the only thing standing between a runaway cursor and an
	// infinite loop. 50 pages * 100 rows = 5,000 trades per member.
	maxPages = 50
)

// Client is a thin wrapper around the Notion API.
type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client

	// retryBase is the first retry delay; it doubles on every attempt. It is a
	// field so tests can run the retry paths without real waiting.
	retryBase time.Duration
}

// NewClient creates a Notion API client.
func NewClient(token string) *Client {
	return &Client{
		token:   token,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
		retryBase: 500 * time.Millisecond,
	}
}

// do executes an HTTP request against the Notion API, retrying transient
// failures.
func (c *Client) do(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		payload = b
	}
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	return c.doWithRetry(ctx, method, path, payload, contentType)
}

// doWithRetry performs the request, retrying on 429, 5xx and network errors.
//
// body is passed as bytes rather than an io.Reader because a reader is consumed
// by the first attempt; rebuilding it from bytes is what makes retries possible.
func (c *Client) doWithRetry(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.waitBeforeRetry(ctx, attempt, lastErr); err != nil {
				return nil, err
			}
		}

		raw, retryable, err := c.attempt(ctx, method, path, body, contentType)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}

	return nil, fmt.Errorf("notion request failed after %d attempts: %w", maxAttempts, lastErr)
}

// attempt performs a single HTTP request. It reports whether the failure is
// worth retrying.
func (c *Client) attempt(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionVersion)
	if contentType != "" && body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A cancelled context is terminal, not transient.
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("http request: %w", err)
		}
		return nil, true, fmt.Errorf("http request: %w", err)
	}

	respBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, true, fmt.Errorf("read response: %w", readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return json.RawMessage(respBody), false, nil
	}

	apiErr := fmt.Errorf("notion API error (status %d): %s", resp.StatusCode, string(respBody))
	if retryAfter := parseRetryAfter(resp); retryAfter > 0 {
		apiErr = &retryAfterError{err: apiErr, after: retryAfter}
	}
	if isRetryableStatus(resp.StatusCode) {
		return nil, true, apiErr
	}
	return nil, false, apiErr
}

// retryAfterError carries a server-provided delay for the next attempt.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// waitBeforeRetry sleeps before the next attempt, honouring Retry-After when
// the server supplied one and otherwise backing off exponentially.
func (c *Client) waitBeforeRetry(ctx context.Context, attempt int, lastErr error) error {
	delay := c.retryBase << (attempt - 2) // 1x, 2x, 4x, ...
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}

	var raErr *retryAfterError
	if errors.As(lastErr, &raErr) && raErr.after > 0 {
		delay = raErr.after
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetryableStatus reports whether an HTTP status is worth another attempt.
// 429 (rate limited) and 5xx (server-side) are transient; 4xx client errors
// such as 400/403/404 are not and must surface immediately.
func isRetryableStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

// parseRetryAfter reads a Retry-After header expressed in seconds.
func parseRetryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// CreatePage creates a new page under a parent.
func (c *Client) CreatePage(ctx context.Context, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/pages", payload)
}

// CreateDatabase creates an inline database inside a page.
func (c *Client) CreateDatabase(ctx context.Context, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/databases", payload)
}

// QueryDatabase queries a single page of a Notion database.
func (c *Client) QueryDatabase(ctx context.Context, dbID string, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/databases/"+dbID+"/query", payload)
}

// pageResponse is the pagination envelope shared by Notion list endpoints.
type pageResponse struct {
	Results    []json.RawMessage `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextCursor *string           `json:"next_cursor"`
}

// QueryDatabaseAll returns every page of a database query.
//
// A single query returns at most 100 rows, so without this the bot would
// silently drop history for any member with more than 100 trades — and would
// then cache the truncated list for 10 minutes.
func (c *Client) QueryDatabaseAll(ctx context.Context, dbID string, payload map[string]interface{}) ([]json.RawMessage, error) {
	return c.paginate(ctx, func(cursor string) (json.RawMessage, error) {
		body := make(map[string]interface{}, len(payload)+2)
		for k, v := range payload {
			body[k] = v
		}
		body["page_size"] = pageSize
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		return c.QueryDatabase(ctx, dbID, body)
	})
}

// SearchPagesAll returns every page of a search query.
//
// Notion returns search results one page at a time, so a single call can miss
// the page the bot is looking for.
func (c *Client) SearchPagesAll(ctx context.Context, query string) ([]json.RawMessage, error) {
	return c.paginate(ctx, func(cursor string) (json.RawMessage, error) {
		return c.search(ctx, query, cursor)
	})
}

func (c *Client) search(ctx context.Context, query, cursor string) (json.RawMessage, error) {
	payload := map[string]interface{}{
		"query": query,
		"filter": map[string]interface{}{
			"value":    "page",
			"property": "object",
		},
		"page_size": pageSize,
	}
	if cursor != "" {
		payload["start_cursor"] = cursor
	}
	return c.do(ctx, http.MethodPost, "/search", payload)
}

// paginate walks a cursor-paginated Notion endpoint until the server reports no
// further pages. Reaching maxPages is reported as an error rather than
// returning a silently truncated result.
func (c *Client) paginate(ctx context.Context, fetch func(cursor string) (json.RawMessage, error)) ([]json.RawMessage, error) {
	var all []json.RawMessage
	cursor := ""

	for range maxPages {
		raw, err := fetch(cursor)
		if err != nil {
			return nil, err
		}

		var resp pageResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("parse page response: %w", err)
		}
		all = append(all, resp.Results...)

		if !resp.HasMore || resp.NextCursor == nil || *resp.NextCursor == "" {
			return all, nil
		}
		cursor = *resp.NextCursor
	}

	return nil, fmt.Errorf("notion pagination exceeded %d pages", maxPages)
}

// UpdatePage updates properties of a Notion page.
func (c *Client) UpdatePage(ctx context.Context, pageID string, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPatch, "/pages/"+pageID, payload)
}

// AppendBlocks appends content blocks (e.g. images) to a page.
func (c *Client) AppendBlocks(ctx context.Context, pageID string, children []map[string]interface{}) (json.RawMessage, error) {
	payload := map[string]interface{}{
		"children": children,
	}
	return c.do(ctx, http.MethodPatch, "/blocks/"+pageID+"/children", payload)
}

// GetPage retrieves a page by ID.
func (c *Client) GetPage(ctx context.Context, pageID string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/pages/"+pageID, nil)
}

// GetBlockChildrenAll returns every direct child block of a page or block.
//
// A page with more than 100 children (a busy parent page, for example) would
// otherwise be read only partially, which for the reset flow means member
// journals left behind in Notion.
func (c *Client) GetBlockChildrenAll(ctx context.Context, blockID string) ([]json.RawMessage, error) {
	return c.paginate(ctx, func(cursor string) (json.RawMessage, error) {
		path := fmt.Sprintf("/blocks/%s/children?page_size=%d", blockID, pageSize)
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		return c.do(ctx, http.MethodGet, path, nil)
	})
}

// CreateFileUpload creates a new single-part Notion file upload object and returns its ID.
func (c *Client) CreateFileUpload(ctx context.Context, filename, contentType string) (string, error) {
	payload := map[string]interface{}{
		"filename":     filename,
		"content_type": contentType,
	}
	raw, err := c.do(ctx, http.MethodPost, "/file_uploads", payload)
	if err != nil {
		return "", err
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse file upload response: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("empty file upload id from Notion")
	}
	return resp.ID, nil
}

// SendFileUpload uploads the binary file contents to an existing Notion file upload object.
func (c *Client) SendFileUpload(ctx context.Context, uploadID, filename, contentType string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create multipart part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write file data: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	path := fmt.Sprintf("/file_uploads/%s/send", uploadID)
	if _, err := c.doWithRetry(ctx, http.MethodPost, path, buf.Bytes(), mw.FormDataContentType()); err != nil {
		return fmt.Errorf("send file upload: %w", err)
	}
	return nil
}
