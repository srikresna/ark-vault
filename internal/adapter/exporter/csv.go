package exporter

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// CSVExporter implements ports.Exporter for CSV output.
type CSVExporter struct{}

// sanitizeCSVField prefixes risky characters to prevent formula injection.
// Excel/Google Sheets can execute formulas starting with =, +, -, @
func sanitizeCSVField(s string) string {
	if s == "" {
		return s
	}
	// Check for characters that could start a formula
	riskyChars := []byte{'=', '+', '-', '@', '\t', '\r', '\n'}
	firstChar := s[0]
	for _, c := range riskyChars {
		if firstChar == c {
			// Prefix with single quote to prevent formula execution
			return "'" + s
		}
	}
	return s
}

// NewCSVExporter creates a CSVExporter.
func NewCSVExporter() *CSVExporter {
	return &CSVExporter{}
}

// ExportCSV produces a CSV byte slice for the given trades.
func (e *CSVExporter) ExportCSV(_ context.Context, trades []domain.Trade) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	// Header
	header := []string{
		"Date", "Asset Type", "Symbol", "Direction",
		"Status", "Result RR", "Time Window",
		"Confluence", "Notes",
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}

	for _, t := range trades {
		row := []string{
			t.Date.Format("2006-01-02"),
			t.AssetType.String(),
			sanitizeCSVField(t.Symbol),
			string(t.Direction),
			string(t.Status),
			fmt.Sprintf("%.2f", t.ResultRR),
			sanitizeCSVField(string(t.TimeWindow)),
			sanitizeCSVField(t.Confluence),
			sanitizeCSVField(t.Notes),
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}

	w.Flush()
	return buf.Bytes(), w.Error()
}
