package domain

import "strings"

// AssetType represents the category of a traded instrument.
type AssetType string

const (
	AssetForex     AssetType = "FOREX"
	AssetGold      AssetType = "GOLD"
	AssetIndices   AssetType = "INDICES"
	AssetCrypto    AssetType = "CRYPTO"
	AssetCommodity AssetType = "COMMODITIES"
)

// ValidAssetTypes lists all recognised asset types.
var ValidAssetTypes = []AssetType{AssetForex, AssetGold, AssetIndices, AssetCrypto, AssetCommodity}

// IsValid returns true when the asset type is one of the known constants.
func (a AssetType) IsValid() bool {
	for _, v := range ValidAssetTypes {
		if a == v {
			return true
		}
	}
	return false
}

// String returns a human-friendly label.
func (a AssetType) String() string {
	switch a {
	case AssetForex:
		return "Forex"
	case AssetGold:
		return "Gold"
	case AssetIndices:
		return "Indices"
	case AssetCrypto:
		return "Crypto"
	case AssetCommodity:
		return "Commodities"
	default:
		return string(a)
	}
}

// ParseAssetType normalizes a user string into a recognised AssetType.
func ParseAssetType(s string) (AssetType, bool) {
	norm := strings.ToUpper(strings.TrimSpace(s))
	switch norm {
	case "FOREX", "FX", "CURRENCY":
		return AssetForex, true
	case "GOLD", "XAU":
		return AssetGold, true
	case "INDICES", "INDEX", "INDEXES":
		return AssetIndices, true
	case "CRYPTO", "CRYPTOCURRENCY", "COIN":
		return AssetCrypto, true
	case "COMMODITY", "COMMODITIES", "OIL":
		return AssetCommodity, true
	default:
		return "", false
	}
}

// knownSymbols maps common pairs/symbols → asset type.
var knownSymbols = map[string]AssetType{
	// Gold & Metals
	"XAU": AssetGold, "XAUUSD": AssetGold, "GOLD": AssetGold, "GOLDUSD": AssetGold,

	// Indices & Futures
	"NQ": AssetIndices, "MNQ": AssetIndices, "NQ1!": AssetIndices, "USTEC": AssetIndices, "NAS100": AssetIndices, "NASDAQ": AssetIndices, "NDX": AssetIndices,
	"ES": AssetIndices, "MES": AssetIndices, "ES1!": AssetIndices, "SPX": AssetIndices, "SPX500": AssetIndices, "US500": AssetIndices, "SP500": AssetIndices,
	"YM": AssetIndices, "MYM": AssetIndices, "YM1!": AssetIndices, "US30": AssetIndices, "DJI": AssetIndices, "DOW": AssetIndices,
	"RTY": AssetIndices, "M2K": AssetIndices, "US2000": AssetIndices,
	"DAX": AssetIndices, "FDAX": AssetIndices, "DE40": AssetIndices, "GER40": AssetIndices, "GER30": AssetIndices,
	"FTSE": AssetIndices, "UK100": AssetIndices,
	"JP225": AssetIndices, "NIKKEI": AssetIndices, "NI225": AssetIndices,
	"HSI": AssetIndices, "HK50": AssetIndices,
	"VIX": AssetIndices,

	// Commodities
	"USOIL": AssetCommodity, "UKOIL": AssetCommodity, "WTI": AssetCommodity, "BRENT": AssetCommodity,
	"CL": AssetCommodity, "QM": AssetCommodity, "XTIUSD": AssetCommodity, "XBRUSD": AssetCommodity,
	"NGAS": AssetCommodity, "NATGAS": AssetCommodity,
	"SILVER": AssetCommodity, "XAGUSD": AssetCommodity, "XAG": AssetCommodity, "COPPER": AssetCommodity,

	// Crypto
	"BTC": AssetCrypto, "BTCUSD": AssetCrypto, "BTCUSDT": AssetCrypto,
	"ETH": AssetCrypto, "ETHUSD": AssetCrypto, "ETHUSDT": AssetCrypto,
	"SOL": AssetCrypto, "SOLUSD": AssetCrypto, "SOLUSDT": AssetCrypto,
	"BNB": AssetCrypto, "BNBUSD": AssetCrypto, "BNBUSDT": AssetCrypto,
	"XRP": AssetCrypto, "XRPUSD": AssetCrypto, "XRPUSDT": AssetCrypto,
	"DOGE": AssetCrypto, "DOGEUSD": AssetCrypto, "DOGEUSDT": AssetCrypto,

	// Forex Common Shorthands
	"EU": AssetForex, "GU": AssetForex, "AU": AssetForex, "NU": AssetForex,
	"UJ": AssetForex, "UC": AssetForex, "UCHF": AssetForex, "UCAD": AssetForex,
	"GJ": AssetForex, "EJ": AssetForex, "AJ": AssetForex, "NJ": AssetForex, "CJ": AssetForex,
	"EG": AssetForex, "EA": AssetForex, "GA": AssetForex, "GN": AssetForex, "AN": AssetForex,
}

func cleanSymbol(symbol string) string {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	sym = strings.TrimSuffix(sym, "!")
	sym = strings.TrimSuffix(sym, "1")
	return sym
}

// DetectAssetType guesses the asset type from a symbol string.
// Falls back to Forex if unrecognised.
func DetectAssetType(symbol string) AssetType {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if at, ok := knownSymbols[sym]; ok {
		return at
	}
	// Try cleaned symbol without continuous futures suffix (e.g. NQ1! -> NQ)
	cleaned := cleanSymbol(sym)
	if at, ok := knownSymbols[cleaned]; ok {
		return at
	}

	// Heuristics:
	// 1. Gold patterns (XAU, GOLD)
	if strings.HasPrefix(sym, "XAU") || strings.HasPrefix(sym, "GOLD") {
		return AssetGold
	}

	// 2. Commodity patterns (OIL, BRENT, WTI, XAG, SILVER, GAS)
	if strings.Contains(sym, "OIL") || strings.Contains(sym, "BRENT") || strings.Contains(sym, "WTI") ||
		strings.HasPrefix(sym, "XAG") || strings.HasPrefix(sym, "XTI") || strings.HasPrefix(sym, "XBR") {
		return AssetCommodity
	}

	// 3. Crypto quote currencies (USDT, BUSD, USDC)
	if strings.HasSuffix(sym, "USDT") || strings.HasSuffix(sym, "BUSD") || strings.HasSuffix(sym, "USDC") {
		return AssetCrypto
	}

	// 4. Heuristic: 6-char alphabetic → likely forex pair
	if len(sym) == 6 && isAllAlpha(sym) {
		return AssetForex
	}
	return AssetForex
}

func isAllAlpha(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
