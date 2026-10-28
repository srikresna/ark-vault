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
	payload, contentType, err := encodeBody(body)
	if err != nil {
		return nil, err
	}
	return c.doWithPolicy(ctx, method, path, payload, contentType, retryTransient)
}

// doCreate performs a non-idempotent create.
//
// Unlike do it retries only 429 responses, which Notion returns without applying
// the request. A network failure, a 5xx, or an unreadable response leaves it
// unknown whether the object was created, and retrying then creates a second
// one: two inline databases split a member's trade history, and two member
// pages orphan one of them. The failure is reported as ambiguous so callers can
// reconcile by reading Notion instead of blindly retrying.
func (c *Client) doCreate(ctx context.Context, path string, body interface{}) (json.RawMessage, error) {
	payload, contentType, err := encodeBody(body)
	if err != nil {
		return nil, err
	}
	return c.doWithPolicy(ctx, http.MethodPost, path, payload, contentType, retryRateLimitOnly)
}

func encodeBody(body interface{}) ([]byte, string, error) {
	if body == nil {
		return nil, "", nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("marshal body: %w", err)
	}
	return payload, "application/json", nil
}

// retryPolicy says which failures a request may be retried after.
type retryPolicy int

const (
	// retryTransient applies to idempotent requests: 429, 5xx and network
	// failures are all safe to repeat.
	retryTransient retryPolicy = iota
	// retryRateLimitOnly applies to non-idempotent creates: only 429 is
	// guaranteed not to have applied the request.
	retryRateLimitOnly
)

// doWithRetry performs the request, retrying on 429, 5xx and network errors.
//
// body is passed as bytes rather than an io.Reader because a reader is consumed
// by the first attempt; rebuilding it from bytes is what makes retries possible.
func (c *Client) doWithRetry(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, error) {
	return c.doWithPolicy(ctx, method, path, body, contentType, retryTransient)
}

func (c *Client) doWithPolicy(ctx context.Context, method, path string, body []byte, contentType string, policy retryPolicy) (json.RawMessage, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := c.waitBeforeRetry(ctx, attempt, lastErr); err != nil {
				return nil, err
			}
		}

		raw, kind, err := c.attempt(ctx, method, path, body, contentType)
		switch kind {
		case attemptOK:
			return raw, nil
		case attemptRetryRateLimit:
			// 429: the request was rejected, never applied, safe for any
			// request including creates.
		case attemptAmbiguous:
			// A create may have been applied, so it is never retried; a
			// cancelled request is not retried either because the outcome is
			// already unknown and the caller is shutting down.
			if policy == retryRateLimitOnly {
				return nil, &ambiguousWriteError{err: err}
			}
			if ctx.Err() != nil {
				return nil, err
			}
		case attemptFatal:
			return nil, err
		}

		lastErr = err
	}

	return nil, fmt.Errorf("notion request failed after %d attempts: %w", maxAttempts, lastErr)
}

// attemptResult classifies a single failed request.
type attemptResult int

const (
	attemptOK attemptResult = iota
	// attemptRetryRateLimit: 429; the request was never applied.
	attemptRetryRateLimit
	// attemptAmbiguous: the request may have been applied (network failure,
	// unreadable response, 5xx).
	attemptAmbiguous
	// attemptFatal: the request was rejected (4xx) and was not applied.
	attemptFatal
)

// attempt performs a single HTTP request and classifies any failure.
func (c *Client) attempt(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, attemptResult, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, attemptFatal, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionVersion)
	if contentType != "" && body != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure (including cancellation) does not prove the
		// request was not applied: the write may have reached Notion before
		// the connection broke. Callers decide what an unknown outcome means;
		// retries are gated by the policy in doWithPolicy.
		return nil, attemptAmbiguous, fmt.Errorf("http request: %w", err)
	}

	respBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		// The server answered but the body was lost; a write may have landed.
		return nil, attemptAmbiguous, fmt.Errorf("read response: %w", readErr)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return json.RawMessage(respBody), attemptOK, nil
	}

	apiErr := &APIError{Status: resp.StatusCode, Body: string(respBody)}
	var errOut error = apiErr
	if retryAfter := parseRetryAfter(resp); retryAfter > 0 {
		errOut = &retryAfterError{err: apiErr, after: retryAfter}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate limited: rejected without being applied, safe for any request.
		return nil, attemptRetryRateLimit, errOut
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		// A 5xx may be returned after the write committed.
		return nil, attemptAmbiguous, errOut
	default:
		return nil, attemptFatal, errOut
	}
}

// APIError reports a non-2xx Notion response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("notion API error (status %d): %s", e.Status, e.Body)
}

// MayHaveBeenSaved reports that a server error may have arrived after Notion
// committed the write. Client errors (4xx) mean the request was rejected.
func (e *APIError) MayHaveBeenSaved() bool {
	return e.Status >= 500 && e.Status <= 599
}

// ambiguousWriteError marks a failure where a non-idempotent request may have
// been applied even though no successful response was read.
type ambiguousWriteError struct{ err error }

func (e *ambiguousWriteError) Error() string { return e.err.Error() }
func (e *ambiguousWriteError) Unwrap() error { return e.err }

// MayHaveBeenSaved reports that the write may have been applied.
//
// It is a marker method rather than an exported type so callers in other
// packages (the Telegram handler) can classify the failure through errors.As
// without importing this adapter.
func (e *ambiguousWriteError) MayHaveBeenSaved() bool { return true }

// IsAmbiguousWriteError reports whether a failed non-idempotent write may still
// have been applied by Notion.
//
// Callers must reconcile by reading the parent (does the page/database exist?)
// instead of retrying: a retry creates a second object.
func IsAmbiguousWriteError(err error) bool {
	var ambiguous *ambiguousWriteError
	if errors.As(err, &ambiguous) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500 && apiErr.Status <= 599
	}
	return false
}

// AmbiguousWriteError marks err as a write whose outcome is unknown.
//
// Adapters use it for a 2xx create whose body cannot be used (malformed JSON, no
// object id): the object exists but cannot be addressed, which is exactly the
// "may have been saved" case callers must reconcile instead of retrying.
func AmbiguousWriteError(err error) error {
	if err == nil {
		return nil
	}
	if IsAmbiguousWriteError(err) {
		return err
	}
	return &ambiguousWriteError{err: err}
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
//
// Creates are not retried after ambiguous failures: a retry would create a
// second page. Callers reconcile with IsAmbiguousWriteError instead.
func (c *Client) CreatePage(ctx context.Context, payload map[string]interface{}) (json.RawMessage, error) {
	return c.doCreate(ctx, "/pages", payload)
}

// CreateDatabase creates an inline database inside a page.
//
// Creates are not retried after ambiguous failures: a retry would create a
// second database, splitting the member's trade history across two of them.
func (c *Client) CreateDatabase(ctx context.Context, payload map[string]interface{}) (json.RawMessage, error) {
	return c.doCreate(ctx, "/databases", payload)
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
