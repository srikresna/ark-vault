package gemini

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewAnalyzer(t *testing.T) {
	t.Run("default model fallback", func(t *testing.T) {
		a := NewAnalyzer("api-key-123", "")
		if a.apiKey != "api-key-123" {
			t.Errorf("expected apiKey 'api-key-123', got %q", a.apiKey)
		}
		if a.model != defaultModel {
			t.Errorf("expected model %q, got %q", defaultModel, a.model)
		}
		if a.client == nil {
			t.Error("expected client to be initialized")
		}
	})

	t.Run("custom model assignment", func(t *testing.T) {
		custom := "gemini-1.5-pro"
		a := NewAnalyzer("api-key-456", custom)
		if a.apiKey != "api-key-456" {
			t.Errorf("expected apiKey 'api-key-456', got %q", a.apiKey)
		}
		if a.model != custom {
			t.Errorf("expected model %q, got %q", custom, a.model)
		}
	})
}

func TestAnalyzeTrades(t *testing.T) {
	t.Run("successful response", func(t *testing.T) {
		a := NewAnalyzer("test-key", "test-model")
		validJSON := `{
			"candidates": [
				{
					"content": {
						"parts": [
							{"text": "Trade analysis summary: strong win rate on EURUSD."}
						]
					}
				}
			]
		}`

		a.client = &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost {
					t.Errorf("expected POST, got %s", req.Method)
				}
				if !strings.Contains(req.URL.String(), "test-model") {
					t.Errorf("expected URL to contain model name, got %s", req.URL.String())
				}
				if !strings.Contains(req.URL.String(), "test-key") {
					t.Errorf("expected URL to contain API key, got %s", req.URL.String())
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewBufferString(validJSON)),
					Header:     make(http.Header),
				}, nil
			}),
		}

		res, err := a.AnalyzeTrades(context.Background(), "analyze these trades")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := "Trade analysis summary: strong win rate on EURUSD."
		if res != expected {
			t.Errorf("expected %q, got %q", expected, res)
		}
	})

	t.Run("non-200 error response", func(t *testing.T) {
		a := NewAnalyzer("test-key", "test-model")
		errorPayload := `{"error": "rate limit exceeded"}`

		a.client = &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(bytes.NewBufferString(errorPayload)),
					Header:     make(http.Header),
				}, nil
			}),
		}

		_, err := a.AnalyzeTrades(context.Background(), "analyze prompt")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "gemini API error 429") {
			t.Errorf("expected 429 error message, got: %v", err)
		}
	})

	t.Run("empty response from Gemini", func(t *testing.T) {
		a := NewAnalyzer("test-key", "test-model")
		emptyJSON := `{"candidates": []}`

		a.client = &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewBufferString(emptyJSON)),
					Header:     make(http.Header),
				}, nil
			}),
		}

		_, err := a.AnalyzeTrades(context.Background(), "prompt")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "empty response from Gemini") {
			t.Errorf("expected empty response error, got: %v", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		a := NewAnalyzer("test-key", "test-model")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := a.AnalyzeTrades(ctx, "prompt")
		if err == nil {
			t.Fatal("expected error with cancelled context, got nil")
		}
	})
}
