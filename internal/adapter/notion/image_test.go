package notion

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageRepo_Upload_UsesNotionFileUpload(t *testing.T) {
	var createdUpload bool
	var sentMultipart bool
	var appendedBlock map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file_uploads":
			createdUpload = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"file_upload","id":"upload-123","status":"pending"}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v1/file_uploads/upload-123/send":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
				t.Errorf("expected multipart/form-data Content-Type, got %q", r.Header.Get("Content-Type"))
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				t.Errorf("expected form file 'file': %v", err)
			} else {
				defer func() { _ = f.Close() }()
				b, _ := io.ReadAll(f)
				if string(b) == "fake-jpeg-bytes" && hdr.Filename == "screenshot_page1.jpg" {
					sentMultipart = true
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"file_upload","id":"upload-123","status":"uploaded"}`))

		case r.Method == http.MethodPatch && r.URL.Path == "/v1/blocks/page-1/children":
			var payload struct {
				Children []map[string]interface{} `json:"children"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode append blocks body: %v", err)
			}
			if len(payload.Children) == 1 {
				appendedBlock = payload.Children[0]
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","results":[]}`))

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("test-token")
	client.baseURL = srv.URL + "/v1"
	repo := NewImageRepo(client)

	gotID, err := repo.Upload(context.Background(), "page-1", "screenshot_page1.jpg", []byte("fake-jpeg-bytes"))
	if err != nil {
		t.Fatalf("Upload returned error: %v", err)
	}
	if !createdUpload {
		t.Error("expected POST /v1/file_uploads to be called")
	}
	if !sentMultipart {
		t.Error("expected POST /v1/file_uploads/upload-123/send with multipart file to be called")
	}
	if gotID != "upload-123" {
		t.Errorf("expected returned upload ID 'upload-123', got %q", gotID)
	}
	imgObj, ok := appendedBlock["image"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected image block in children, got %#v", appendedBlock)
	}
	if imgObj["type"] != "file_upload" {
		t.Errorf("expected image.type = 'file_upload', got %v", imgObj["type"])
	}
	fuObj, ok := imgObj["file_upload"].(map[string]interface{})
	if !ok || fuObj["id"] != "upload-123" {
		t.Errorf("expected image.file_upload.id = 'upload-123', got %#v", imgObj["file_upload"])
	}
}
