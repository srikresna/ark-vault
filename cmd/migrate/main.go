// Command migrate copies the pre-Postgres data into Postgres: member journals
// and trades from Notion, gamification state from a BadgerDB copy.
//
// It is a one-shot cutover tool. It refuses to run against a journal that
// already holds trades, so a mistyped second run cannot duplicate the community's
// history.
//
// Usage:
//
//	NOTION_TOKEN=... NOTION_PARENT_PAGE_ID=... DATABASE_URL=... \
//	  go run ./cmd/migrate -badger /path/to/badger-copy [-dry-run]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/arkcode369/ark-vault/internal/importer"
)

func main() {
	badgerDir := flag.String("badger", "", "path to a COPY of the BadgerDB volume (empty skips gamification)")
	imageDir := flag.String("images", "", "screenshot root the new deployment serves from (empty skips screenshots)")
	dryRun := flag.Bool("dry-run", false, "read both sources and report counts without writing")
	timeout := flag.Duration("timeout", 15*time.Minute, "overall deadline")
	flag.Parse()

	opts := importer.Options{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		NotionToken:  os.Getenv("NOTION_TOKEN"),
		NotionParent: os.Getenv("NOTION_PARENT_PAGE_ID"),
		BadgerDir:    *badgerDir,
		ImageDir:     *imageDir,
		DryRun:       *dryRun,
	}
	if opts.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	}
	if opts.NotionToken == "" || opts.NotionParent == "" {
		fail("NOTION_TOKEN and NOTION_PARENT_PAGE_ID are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	report, err := importer.Run(ctx, opts)
	if err != nil {
		fail(err.Error())
	}

	verb := "imported"
	if *dryRun {
		verb = "would import"
	}
	fmt.Printf("%s:\n", verb)
	fmt.Printf("  members    %d\n", report.Members)
	fmt.Printf("  trades     %d\n", report.Trades)
	if report.ImageDir != "" {
		fmt.Printf("  screenshots %d found, %d stored\n", report.Screenshots, report.ScreenshotsStored)
	}
	fmt.Printf("  profiles   %d\n", report.Profiles)
	fmt.Printf("  streaks    %d\n", report.Streaks)
	fmt.Printf("  xp events  %d\n", report.XPEvents)
	fmt.Printf("  xp grants  %d (dropped %d with no surviving trade)\n",
		report.Grants, report.GrantsDropped)
	fmt.Printf("  badges     %d\n", report.Badges)
	fmt.Printf("  goals      %d\n", report.Goals)
	fmt.Printf("  reminders  %d\n", report.Reminders)
	fmt.Printf("  challenges %d\n", report.Challenges)
	if report.UnknownMember > 0 {
		fmt.Printf("  dropped    %d records whose member is not in the journal\n", report.UnknownMember)
	}
	if len(report.Skipped) > 0 {
		fmt.Println("  skipped:")
		for _, k := range report.SkippedKeys() {
			fmt.Printf("    %-18s %d\n", k, report.Skipped[k])
		}
	}
	for _, w := range report.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

func fail(msg string) {
	fmt.Fprintf(os.Stderr, "migrate: %s\n", msg)
	os.Exit(1)
}
