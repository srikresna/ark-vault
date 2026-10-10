package notion

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ImageRepo implements ports.ImageStore by appending image blocks to Notion pages.
type ImageRepo struct {
	client *Client
}

// NewImageRepo creates an ImageRepo.
func NewImageRepo(client *Client) *ImageRepo {
	return &ImageRepo{client: client}
}

// Upload appends an image block to the trade's Notion page.
// If data is provided, it uploads the file via the Notion File Upload API and attaches it as a file_upload image block.
// If data is nil, filename is treated as an external URL.
func (r *ImageRepo) Upload(ctx context.Context, pageID string, filename string, data []byte) (string, error) {
	var imageBlock map[string]interface{}
	var resultRef string

	if len(data) > 0 {
		contentType := "image/jpeg"
		lower := strings.ToLower(filename)
		switch {
		case strings.HasSuffix(lower, ".png"):
			contentType = "image/png"
		case strings.HasSuffix(lower, ".webp"):
			contentType = "image/webp"
		case strings.HasSuffix(lower, ".gif"):
			contentType = "image/gif"
		}

		uploadID, err := r.client.CreateFileUpload(ctx, filename, contentType)
		if err != nil {
			return "", fmt.Errorf("create file upload: %w", err)
		}
		if err := r.client.SendFileUpload(ctx, uploadID, filename, contentType, data); err != nil {
			return "", fmt.Errorf("send file upload: %w", err)
		}

		imageBlock = map[string]interface{}{
			"type": "file_upload",
			"file_upload": map[string]interface{}{
				"id": uploadID,
			},
		}
		resultRef = uploadID
	} else {
		imageBlock = map[string]interface{}{
			"type": "external",
			"external": map[string]interface{}{
				"url": filename,
			},
		}
		resultRef = filename
	}

	children := []map[string]interface{}{
		{
			"object": "block",
			"type":   "image",
			"image":  imageBlock,
		},
	}

	_, err := r.client.AppendBlocks(ctx, pageID, children)
	if err != nil {
		return "", fmt.Errorf("append image block: %w", err)
	}
	return resultRef, nil
}

// parseDate parses a Notion date string.
func parseDate(s string) (time.Time, error) {
	layouts := []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date: %s", s)
}
