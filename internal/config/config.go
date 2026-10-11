package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config holds all application configuration.
type Config struct {
	TelegramToken  string
	NotionToken    string
	NotionParentID string // The parent page ID under which member pages are created
	LogLevel       string

	// Report settings
	ReportChatID    int64  // Group chat ID to post weekly reports to
	ReportThreadID  int    // Topic/thread ID for groups with Topics enabled (0 = General)
	ReportDay       string // Day of week: "monday", "sunday", etc. Default: "sunday"
	ReportHour      int    // Hour (0-23) to post report. Default: 20

	// Community membership gate
	CommunityGroupID int64 // Telegram group ID to check membership against
	OwnerID          int64 // Owner's Telegram user ID for contact link

	// BadgerDB (Gamification Storage)
	BadgerDBPath string

	// Gemini AI
	GeminiAPIKey string
	GeminiModel  string // default "gemini-3.1-flash-lite-preview"

	// Rate limiting
	RateLimitPerMin int // Max commands per user per minute. Default: 10
	// Metrics & Health server (Prometheus exposition format)
	MetricsAddr string // HTTP address to serve /metrics and /healthz. Default: :9102
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	c := &Config{
		TelegramToken:  strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		NotionToken:    strings.TrimSpace(os.Getenv("NOTION_TOKEN")),
		NotionParentID: strings.TrimSpace(os.Getenv("NOTION_PARENT_PAGE_ID")),
		LogLevel:       strings.TrimSpace(os.Getenv("LOG_LEVEL")),
		ReportDay:      strings.TrimSpace(os.Getenv("REPORT_DAY")),
	}

	if c.TelegramToken == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if c.NotionToken == "" {
		return nil, errors.New("NOTION_TOKEN is required")
	}
	if c.NotionParentID == "" {
		return nil, errors.New("NOTION_PARENT_PAGE_ID is required")
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ReportDay == "" {
		c.ReportDay = "sunday"
	}

	if chatStr := os.Getenv("REPORT_CHAT_ID"); chatStr != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(chatStr), 10, 64)
		if err != nil {
			return nil, errors.New("REPORT_CHAT_ID must be a valid integer")
		}
		c.ReportChatID = id
	}

	if threadStr := os.Getenv("REPORT_THREAD_ID"); threadStr != "" {
		tid, err := strconv.Atoi(strings.TrimSpace(threadStr))
		if err != nil {
			return nil, errors.New("REPORT_THREAD_ID must be a valid integer")
		}
		c.ReportThreadID = tid
	}

	if hourStr := os.Getenv("REPORT_HOUR"); hourStr != "" {
		h, err := strconv.Atoi(strings.TrimSpace(hourStr))
		if err != nil || h < 0 || h > 23 {
			return nil, errors.New("REPORT_HOUR must be 0-23")
		}
		c.ReportHour = h
	} else {
		c.ReportHour = 20
	}

	if cgStr := os.Getenv("COMMUNITY_GROUP_ID"); cgStr != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(cgStr), 10, 64)
		if err != nil {
			return nil, errors.New("COMMUNITY_GROUP_ID must be a valid integer")
		}
		c.CommunityGroupID = id
	}

	if ownerStr := os.Getenv("OWNER_ID"); ownerStr != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(ownerStr), 10, 64)
		if err != nil {
			return nil, errors.New("OWNER_ID must be a valid integer")
		}
		c.OwnerID = id
	}

	c.BadgerDBPath = strings.TrimSpace(os.Getenv("BADGER_DB_PATH"))
	if c.BadgerDBPath == "" {
		c.BadgerDBPath = "/data/badger"
	}

	c.GeminiAPIKey = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	c.GeminiModel = strings.TrimSpace(os.Getenv("GEMINI_MODEL"))
	if c.GeminiModel == "" {
		c.GeminiModel = "gemini-3.1-flash-lite-preview"
	}

	if rlStr := os.Getenv("RATE_LIMIT_PER_MIN"); rlStr != "" {
		rl, err := strconv.Atoi(strings.TrimSpace(rlStr))
		if err != nil || rl < 1 {
			return nil, errors.New("RATE_LIMIT_PER_MIN must be a positive integer")
		}
		c.RateLimitPerMin = rl
	} else {
		c.RateLimitPerMin = 10
	}


	c.MetricsAddr = strings.TrimSpace(os.Getenv("METRICS_ADDR"))
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9102"
	}
	return c, nil
}
