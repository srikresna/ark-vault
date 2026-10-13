package exporter

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// PDFExporter generates a simple text-based PDF report.
// Uses a minimal PDF generator without external dependencies.
type PDFExporter struct{}

// NewPDFExporter creates a PDFExporter.
func NewPDFExporter() *PDFExporter {
	return &PDFExporter{}
}

// ExportPDF produces a minimal PDF byte slice for the given trades and stats.
func (e *PDFExporter) ExportPDF(_ context.Context, username string, trades []domain.Trade, stats *domain.Stats) ([]byte, error) {
	var content strings.Builder

	content.WriteString("ARK Vault — Trade Journal Report\n")
	fmt.Fprintf(&content, "Member: @%s\n", username)
	// The generation timestamp is the report date; it must not depend on the
	// first trade's date (and must not panic on an empty slice).
	fmt.Fprintf(&content, "Generated: %s\n", time.Now().In(timeutil.WIB).Format("2006-01-02"))
	content.WriteString(strings.Repeat("=", 50) + "\n\n")

	// Stats summary
	if stats != nil {
		content.WriteString("PERFORMANCE SUMMARY\n")
		content.WriteString(strings.Repeat("-", 30) + "\n")
		fmt.Fprintf(&content, "Total Trades:    %d\n", stats.TotalTrades)
		fmt.Fprintf(&content, "Win/Loss/BE:     %d / %d / %d\n", stats.Wins, stats.Losses, stats.BreakEvens)
		fmt.Fprintf(&content, "Win Rate:        %.1f%%\n", stats.WinRate)
		fmt.Fprintf(&content, "Total RR:        %+.1fR\n", stats.TotalRR)
		fmt.Fprintf(&content, "Avg RR:          %.2f\n", stats.AvgRR)
		fmt.Fprintf(&content, "Best Trade:      %+.1fR\n", stats.BestRR)
		fmt.Fprintf(&content, "Worst Trade:     %+.1fR\n", stats.WorstRR)
		fmt.Fprintf(&content, "Max Win Streak:  %d\n\n", stats.MaxWinStrk)
	}

	// Trade list
	content.WriteString("TRADE HISTORY\n")
	content.WriteString(strings.Repeat("-", 30) + "\n")
	for i, t := range trades {
		fmt.Fprintf(&content, "\n#%d  %s  %s %s\n", i+1, t.Date.Format("2006-01-02"), t.Symbol, t.Direction)
		fmt.Fprintf(&content, "    Status: %s", t.Status)
		if t.ResultRR != 0 {
			fmt.Fprintf(&content, "  RR: %+.1fR", t.ResultRR)
		}
		content.WriteString("\n")
		if t.TimeWindow != "" {
			fmt.Fprintf(&content, "    Session: %s\n", t.TimeWindow)
		}
		if t.Confluence != "" {
			fmt.Fprintf(&content, "    Confluence: %s\n", t.Confluence)
		}
	}

	// Generate minimal valid PDF
	return generateMinimalPDF(content.String()), nil
}

// generateMinimalPDF creates a bare-minimum valid PDF with text content.
func generateMinimalPDF(text string) []byte {
	var buf bytes.Buffer

	lines := strings.Split(text, "\n")

	// PDF Header
	buf.WriteString("%PDF-1.4\n")

	// Object 1: Catalog
	obj1Offset := buf.Len()
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	// Object 2: Pages
	obj2Offset := buf.Len()
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")

	// Object 4: Font
	obj4Offset := buf.Len()
	buf.WriteString("4 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>\nendobj\n")

	// Object 5: Stream content
	var stream bytes.Buffer
	stream.WriteString("BT\n/F1 9 Tf\n")
	y := 780.0
	for _, line := range lines {
		if y < 40 {
			break // simple single-page limit
		}
		escaped := pdfEscape(line)
		fmt.Fprintf(&stream, "1 0 0 1 40 %.0f Tm\n(%s) Tj\n", y, escaped)
		y -= 12
	}
	stream.WriteString("ET\n")

	obj5Offset := buf.Len()
	streamBytes := stream.Bytes()
	fmt.Fprintf(&buf, "5 0 obj\n<< /Length %d >>\nstream\n", len(streamBytes))
	buf.Write(streamBytes)
	buf.WriteString("\nendstream\nendobj\n")

	// Object 3: Page
	obj3Offset := buf.Len()
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 5 0 R /Resources << /Font << /F1 4 0 R >> >> >>\nendobj\n")

	// Cross-reference table
	xrefOffset := buf.Len()
	buf.WriteString("xref\n0 6\n")
	buf.WriteString("0000000000 65535 f \n")
	fmt.Fprintf(&buf, "%010d 00000 n \n", obj1Offset)
	fmt.Fprintf(&buf, "%010d 00000 n \n", obj2Offset)
	fmt.Fprintf(&buf, "%010d 00000 n \n", obj3Offset)
	fmt.Fprintf(&buf, "%010d 00000 n \n", obj4Offset)
	fmt.Fprintf(&buf, "%010d 00000 n \n", obj5Offset)

	// Trailer
	fmt.Fprintf(&buf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return buf.Bytes()
}

func pdfEscape(s string) string {
	var buf strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			buf.WriteString("\\\\")
		case '(':
			buf.WriteString("\\(")
		case ')':
			buf.WriteString("\\)")
		case '\r':
			buf.WriteString("\\r")
		case '\n':
			buf.WriteString("\\n")
		case '\t':
			buf.WriteString("\\t")
		case '\b':
			buf.WriteString("\\b")
		case '\f':
			buf.WriteString("\\f")
		default:
			// Control characters (0-31 except whitespace, and 127) should be escaped
			if r < 32 || r == 127 {
				fmt.Fprintf(&buf, "\\%03o", r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	return buf.String()
}
