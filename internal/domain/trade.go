package domain

import (
	"errors"
	"time"
)

// Direction of a trade.
type Direction string

const (
	DirBuy  Direction = "BUY"
	DirSell Direction = "SELL"
)

// TradeStatus captures the lifecycle state of a trade.
type TradeStatus string

const (
	StatusOpen TradeStatus = "OPEN"
	StatusWin  TradeStatus = "WIN"
	StatusLoss TradeStatus = "LOSS"
	StatusBE   TradeStatus = "BE" // break-even
)

// TimeWindow represents the trading session.
type TimeWindow string

const (
	SessionAsia   TimeWindow = "Asia"
	SessionLondon TimeWindow = "London"
	SessionNYAM   TimeWindow = "NY AM"
	SessionNYPM   TimeWindow = "NY PM"
)

// Trade is the core domain entity for a single trade journal entry.
type Trade struct {
	ID            string // Notion page ID of the trade entry
	MemberID      int64  // Telegram user ID
	Date          time.Time
	CreatedAt     time.Time // Notion page creation time; tie-breaker for "latest trade"
	AssetType     AssetType
	Symbol        string
	Direction     Direction
	ResultRR      float64    // R multiple result (e.g. +2, -1, 0 for BE)
	Status        TradeStatus
	TimeWindow    TimeWindow
	Confluence    string
	Notes         string
	ScreenshotURL string // URL after uploaded to Notion
}

// Validate performs basic checks on the trade before persisting.
func (t *Trade) Validate() error {
	if t.Symbol == "" {
		return errors.New("symbol is required")
	}
	if t.Direction != DirBuy && t.Direction != DirSell {
		return errors.New("direction must be BUY or SELL")
	}
	return nil
}

// AutoDetectAsset sets AssetType from Symbol if not already set.
func (t *Trade) AutoDetectAsset() {
	if !t.AssetType.IsValid() || t.AssetType == "" {
		t.AssetType = DetectAssetType(t.Symbol)
	}
}
