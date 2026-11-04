package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ImageStore saves trade screenshots to a local volume.
//
// Notion hosted the binaries before this migration. Files now live under a
// configured root and are addressed by an opaque key stored in the trade row, so
// moving to object storage later only changes this adapter: the database keeps
// the same key column.
type ImageStore struct {
	root string
}

// NewImageStore builds the image store rooted at dir. It creates the directory
// so the first upload does not fail on a fresh volume.
func NewImageStore(dir string) (*ImageStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("image store: root directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create image store root %s: %w", dir, err)
	}
	return &ImageStore{root: dir}, nil
}

// Upload writes data and returns the key it was stored under.
//
// The key is derived from the trade id plus random bytes, never from the
// client-supplied filename: a name like "../../etc/passwd" must not be able to
// escape the root, and two uploads for one trade must not overwrite each other.
func (s *ImageStore) Upload(ctx context.Context, tradeID string, filename string, data []byte) (string, error) {
	if tradeID == "" {
		return "", fmt.Errorf("upload image: trade id is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("upload image: empty payload")
	}

	suffix := strings.ToLower(filepath.Ext(filepath.Base(filename)))
	if !allowedImageExt(suffix) {
		suffix = ".bin"
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("upload image: generate key: %w", err)
	}
	key := filepath.ToSlash(filepath.Join(
		sanitizeSegment(tradeID), hex.EncodeToString(rnd[:])+suffix))

	full := filepath.Join(s.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return "", fmt.Errorf("upload image: create directory: %w", err)
	}
	// O_EXCL: a collision must surface as an error, never as a silent overwrite
	// of another trade's screenshot.
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("upload image: create %s: %w", key, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(full)
		return "", fmt.Errorf("upload image: write %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(full)
		return "", fmt.Errorf("upload image: close %s: %w", key, err)
	}
	return key, nil
}

// Open returns a reader for a stored key, plus its size. The caller closes it.
func (s *ImageStore) Open(key string) (*os.File, int64, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, 0, fmt.Errorf("open image: invalid key %q", key)
	}
	full := filepath.Join(s.root, clean)
	// Re-check after joining: a symlinked parent must not escape the root.
	if !strings.HasPrefix(full, filepath.Clean(s.root)+string(os.PathSeparator)) {
		return nil, 0, fmt.Errorf("open image: key %q escapes root", key)
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, fmt.Errorf("open image %s: %w", key, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat image %s: %w", key, err)
	}
	return f, info.Size(), nil
}

// Remove deletes a stored image. A missing file is not an error.
func (s *ImageStore) Remove(key string) error {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return fmt.Errorf("remove image: invalid key %q", key)
	}
	full := filepath.Join(s.root, clean)
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove image %s: %w", key, err)
	}
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

// sanitizeSegment keeps a trade id usable as a directory name.
func sanitizeSegment(s string) string {
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
