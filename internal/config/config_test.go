package config

import (
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"TELEGRAM_BOT_TOKEN",
		"NOTION_TOKEN",
		"NOTION_PARENT_PAGE_ID",
		"LOG_LEVEL",
		"REPORT_DAY",
		"REPORT_CHAT_ID",
		"REPORT_THREAD_ID",
		"REPORT_HOUR",
		"COMMUNITY_GROUP_ID",
		"OWNER_ID",
		"BADGER_DB_PATH",
		"GEMINI_API_KEY",
		"GEMINI_MODEL",
		"RATE_LIMIT_PER_MIN",
		"METRICS_ADDR",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestLoad_ValidDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "mock-telegram-token")
	t.Setenv("NOTION_TOKEN", "mock-notion-token")
	t.Setenv("NOTION_PARENT_PAGE_ID", "mock-parent-page-id")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.TelegramToken != "mock-telegram-token" {
		t.Errorf("expected TelegramToken to be 'mock-telegram-token', got %q", cfg.TelegramToken)
	}
	if cfg.NotionToken != "mock-notion-token" {
		t.Errorf("expected NotionToken to be 'mock-notion-token', got %q", cfg.NotionToken)
	}
	if cfg.NotionParentID != "mock-parent-page-id" {
		t.Errorf("expected NotionParentID to be 'mock-parent-page-id', got %q", cfg.NotionParentID)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected LogLevel 'info', got %q", cfg.LogLevel)
	}
	if cfg.ReportDay != "sunday" {
		t.Errorf("expected ReportDay 'sunday', got %q", cfg.ReportDay)
	}
	if cfg.ReportHour != 20 {
		t.Errorf("expected ReportHour 20, got %d", cfg.ReportHour)
	}
	if cfg.BadgerDBPath != "/data/badger" {
		t.Errorf("expected BadgerDBPath '/data/badger', got %q", cfg.BadgerDBPath)
	}
	if cfg.RateLimitPerMin != 10 {
		t.Errorf("expected RateLimitPerMin 10, got %d", cfg.RateLimitPerMin)
	}
	if cfg.MetricsAddr != ":9102" {
		t.Errorf("expected MetricsAddr ':9102', got %q", cfg.MetricsAddr)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
	t.Setenv("NOTION_TOKEN", "ntok")
	t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("REPORT_DAY", "monday")
	t.Setenv("REPORT_CHAT_ID", "-100123")
	t.Setenv("REPORT_THREAD_ID", "42")
	t.Setenv("REPORT_HOUR", "9")
	t.Setenv("COMMUNITY_GROUP_ID", "-100456")
	t.Setenv("OWNER_ID", "789")
	t.Setenv("BADGER_DB_PATH", "/tmp/badger")
	t.Setenv("GEMINI_API_KEY", "gem-key")
	t.Setenv("GEMINI_MODEL", "gemini-pro")
	t.Setenv("RATE_LIMIT_PER_MIN", "15")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != "debug" || cfg.ReportDay != "monday" || cfg.ReportHour != 9 ||
		cfg.ReportChatID != -100123 || cfg.ReportThreadID != 42 ||
		cfg.CommunityGroupID != -100456 || cfg.OwnerID != 789 ||
		cfg.BadgerDBPath != "/tmp/badger" || cfg.GeminiAPIKey != "gem-key" ||
		cfg.GeminiModel != "gemini-pro" || cfg.RateLimitPerMin != 15 {
		t.Errorf("unexpected loaded config: %+v", cfg)
	}
}

func TestLoad_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T)
		errSubstr string
	}{
		{
			name: "missing TELEGRAM_BOT_TOKEN",
			setup: func(t *testing.T) {
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
			},
			errSubstr: "TELEGRAM_BOT_TOKEN is required",
		},
		{
			name: "missing NOTION_TOKEN",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
			},
			errSubstr: "NOTION_TOKEN is required",
		},
		{
			name: "missing NOTION_PARENT_PAGE_ID",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
			},
			errSubstr: "NOTION_PARENT_PAGE_ID is required",
		},
		{
			name: "invalid REPORT_CHAT_ID",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("REPORT_CHAT_ID", "abc")
			},
			errSubstr: "REPORT_CHAT_ID must be a valid integer",
		},
		{
			name: "invalid REPORT_THREAD_ID",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("REPORT_THREAD_ID", "not-an-int")
			},
			errSubstr: "REPORT_THREAD_ID must be a valid integer",
		},
		{
			name: "invalid REPORT_HOUR > 23",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("REPORT_HOUR", "25")
			},
			errSubstr: "REPORT_HOUR must be 0-23",
		},
		{
			name: "invalid REPORT_HOUR negative",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("REPORT_HOUR", "-1")
			},
			errSubstr: "REPORT_HOUR must be 0-23",
		},
		{
			name: "invalid RATE_LIMIT_PER_MIN zero",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("RATE_LIMIT_PER_MIN", "0")
			},
			errSubstr: "RATE_LIMIT_PER_MIN must be a positive integer",
		},
		{
			name: "invalid RATE_LIMIT_PER_MIN negative",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("RATE_LIMIT_PER_MIN", "-5")
			},
			errSubstr: "RATE_LIMIT_PER_MIN must be a positive integer",
		},
		{
			name: "invalid RATE_LIMIT_PER_MIN non-numeric",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("RATE_LIMIT_PER_MIN", "foo")
			},
			errSubstr: "RATE_LIMIT_PER_MIN must be a positive integer",
		},
		{
			name: "invalid COMMUNITY_GROUP_ID",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("COMMUNITY_GROUP_ID", "xyz")
			},
			errSubstr: "COMMUNITY_GROUP_ID must be a valid integer",
		},
		{
			name: "invalid OWNER_ID",
			setup: func(t *testing.T) {
				t.Setenv("TELEGRAM_BOT_TOKEN", "tok")
				t.Setenv("NOTION_TOKEN", "tok")
				t.Setenv("NOTION_PARENT_PAGE_ID", "parent")
				t.Setenv("OWNER_ID", "bad")
			},
			errSubstr: "OWNER_ID must be a valid integer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			tc.setup(t)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSubstr)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("expected error containing %q, got %q", tc.errSubstr, err.Error())
			}
		})
	}
}
