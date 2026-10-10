package exporter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

func TestSanitizeCSVField(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"no risk", "BTC", "BTC"},
		{"equals sign (formula)", "=CMD|'/C calc'!A0", "'=CMD|'/C calc'!A0"},
		{"plus sign", "+cmd", "'+cmd"},
		{"minus sign", "-cmd", "'-cmd"},
		{"at sign", "@cmd", "'@cmd"},
		{"tab start", "\tformula", "'\tformula"},
		{"carriage return", "\rformula", "'\rformula"},
		{"newline", "\nformula", "'\nformula"},
		{"empty string", "", ""},
		{"normal text with equals", "result=5", "result=5"},
		{"safe text", "Normal trade note", "Normal trade note"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeCSVField(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeCSVField(%q) = %q; want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestCSVExporter_ExportCSV_Sanitization(t *testing.T) {
	exporter := NewCSVExporter()
	ctx := context.Background()

	trades := []domain.Trade{
		{
			Date:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			AssetType:  domain.AssetCrypto,
			Symbol:     "=CMD|'/C calc'!A0", // Malicious formula
			Direction:  domain.DirBuy,
			Status:     domain.StatusWin,
			ResultRR:   2.5,
			TimeWindow: "NY",
			Confluence: "+CMD", // Another risky prefix
			Notes:      "- malicious - note @cmd",
		},
	}

	data, err := exporter.ExportCSV(ctx, trades)
	if err != nil {
		t.Fatalf("ExportCSV failed: %v", err)
	}

	csvContent := string(data)

	// Verify formula characters are prefixed with single quote
	if !strings.Contains(csvContent, "'=CMD|'/C calc'!A0") {
		t.Error("CSV should contain sanitized formula (with leading single quote)")
	}
	if !strings.Contains(csvContent, "'+CMD") {
		t.Error("CSV should contain sanitized + prefix (with leading single quote)")
	}
	if !strings.Contains(csvContent, "'- malicious - note @cmd") {
		t.Error("CSV should contain sanitized - and @ prefixes (with leading single quote)")
	}

	// Verify the raw dangerous formula is NOT present
	if strings.Contains(csvContent, ",=CMD|") {
		t.Error("CSV contains raw formula - sanitization failed!")
	}
}
