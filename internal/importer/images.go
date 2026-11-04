package importer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// maxImageBytes caps one downloaded screenshot. A phone photo is a few
// megabytes; the cap only exists so a wrong URL cannot exhaust the machine.
const maxImageBytes = 15 << 20

// downloadImages fetches the signed Notion URLs captured by AttachImages into a
// temporary directory.
//
// It runs before the database transaction on purpose: 33 HTTP round trips would
// otherwise hold one transaction open for minutes, and a failure here must abort
// the migration before anything is written.
func downloadImages(ctx context.Context, dir string, trades []Trade) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create image staging dir: %w", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}

	for i := range trades {
		url := trades[i].ImageURL
		if url == "" {
			continue
		}
		if err := fetchImage(ctx, client, dir, &trades[i], url); err != nil {
			return fmt.Errorf("download screenshot of trade %s: %w", trades[i].NotionPageID, err)
		}
	}
	return nil
}

func fetchImage(ctx context.Context, client *http.Client, dir string, t *Trade, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	// The extension decides the served content type, so take it from the URL
	// path and fall back to the response type, never from the raw URL query.
	ext := strings.ToLower(path.Ext(strings.Split(url, "?")[0]))
	if !allowedImageExt(ext) {
		ext = extensionForContentType(resp.Header.Get("Content-Type"))
	}
	file := filepath.Join(dir, sanitizeFilename(t.NotionPageID)+ext)
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, maxImageBytes))
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(file)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(file)
		return closeErr
	}
	if written == 0 {
		_ = os.Remove(file)
		return fmt.Errorf("empty image body")
	}
	t.ImagePath = file
	return nil
}

func allowedImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
}

func extensionForContentType(ct string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}

// sanitizeFilename keeps a Notion page id usable as a file name.
func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "trade"
	}
	return b.String()
}
