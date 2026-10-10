package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"
)

const (
	baseURL        = "https://api.notion.com/v1"
	notionVersion  = "2022-06-28"
	requestTimeout = 30 * time.Second
)

// Client is a thin wrapper around the Notion API.
type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Notion API client.
func NewClient(token string) *Client {
	return &Client{
		token:   token,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
	}
}

// do executes an HTTP request against the Notion API.
func (c *Client) do(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("notion API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return json.RawMessage(respBody), nil
}

// CreatePage creates a new page under a parent.
func (c *Client) CreatePage(ctx context.Context, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/pages", payload)
}

// CreateDatabase creates an inline database inside a page.
func (c *Client) CreateDatabase(ctx context.Context, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/databases", payload)
}

// QueryDatabase queries a Notion database with optional filter/sort.
func (c *Client) QueryDatabase(ctx context.Context, dbID string, payload map[string]interface{}) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/databases/"+dbID+"/query", payload)
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

// GetBlockChildren retrieves direct child blocks of a page or block.
func (c *Client) GetBlockChildren(ctx context.Context, blockID string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/blocks/"+blockID+"/children?page_size=100", nil)
}

// SearchPages searches for pages with a given title query.
func (c *Client) SearchPages(ctx context.Context, query string) (json.RawMessage, error) {
	payload := map[string]interface{}{
		"query": query,
		"filter": map[string]interface{}{
			"value":    "page",
			"property": "object",
		},
	}
	return c.do(ctx, http.MethodPost, "/search", payload)
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

	url := fmt.Sprintf("%s/file_uploads/%s/send", c.baseURL, uploadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return fmt.Errorf("create send file request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionVersion)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send file request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read send file response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notion file upload send error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}
