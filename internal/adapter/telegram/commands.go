package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Command is the single source of truth for a bot command. Routing, the
// Telegram command menu, and the /help listing are all derived from
// commandCatalog, so a command can no longer be reachable but undocumented, or
// documented and registered but unreachable.
type Command struct {
	Name      string   // without the leading slash
	Group     string   // must match a helpSections title
	Desc      string   // Telegram command-menu description
	Help      []string // /help bullet lines, without the leading "• "
	OwnerOnly bool
	Run       func(h *Handler, ctx context.Context, msg *Message, text string)
}

const (
	groupTrade  = "Mencatat &amp; Kelola Trade:"
	groupStats  = "Statistik &amp; Ranking:"
	groupExport = "Export &amp; Report:"
	groupOther  = "Lainnya:"
	groupOwner  = "Owner Only:"
)

// journalFormatNote documents the passive #journal message format. It is a note
// of the trade group rather than a command: #journal is not a slash command and
// never appears in the command menu.
const journalFormatNote = `• Kirim pesan dengan format:
<pre>#journal
Pair: XAUUSD
Type: BUY
RR: +2
Session: London
Confluence: FVG + OB mitigation on 15m</pre>
  Bisa attach screenshot bersamaan.
`

const helpTips = `<b>Tips:</b>
• RR bisa: +2, -1, 0 (BE), atau WIN 2RR, LOSS 1RR, BE
• Kalau RR kosong, trade dicatat sebagai OPEN
• Session: Asia, London, NY AM, NY PM`

// helpSections fixes the display order and titles of the /help listing.
var helpSections = []struct {
	title string
	note  string
}{
	{groupTrade, journalFormatNote},
	{groupStats, ""},
	{groupExport, ""},
	{groupOther, ""},
}

// commandCatalog lists every routable command exactly once.
//
// It is a function rather than a package-level variable on purpose: the catalog
// holds closures that render help from the catalog itself, and Go rejects that
// as an initialization cycle when both ends are package-level variables.
func commandCatalog() []Command {
	return []Command{
		{
			Name: "journal", Group: groupTrade,
			Desc: "Catat trade (guided flow)",
			Help: []string{"/journal — Guided flow (step-by-step dengan tombol)"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdJournal(ctx, msg) },
		},
		{
			Name: "open", Group: groupTrade,
			Desc: "Lihat & tutup trade yang masih OPEN",
			Help: []string{"/open — Lihat &amp; tutup trade yang masih OPEN"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdOpen(ctx, msg) },
		},
		{
			Name: "history", Group: groupTrade,
			Desc: "Riwayat trade (Edit & Hapus)",
			Help: []string{"/history — Riwayat trade (bisa Edit &amp; Hapus trade mana saja)"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdHistory(ctx, msg) },
		},
		{
			Name: "undo", Group: groupTrade,
			Desc: "Hapus 1 trade terakhir",
			Help: []string{"/undo — Hapus 1 trade terakhir yang baru diinput"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdUndo(ctx, msg) },
		},
		{
			Name: "stats", Group: groupStats,
			Desc: "Statistik trading (all/week/month)",
			Help: []string{
				"/stats [week|month] — Statistik personal",
				"/stats @user [week|month] — Statistik member lain",
			},
			Run: func(h *Handler, ctx context.Context, msg *Message, text string) { h.cmdStats(ctx, msg, text) },
		},
		{
			Name: "leaderboard", Group: groupStats,
			Desc: "Top 10 trader komunitas",
			Help: []string{
				"/leaderboard [week|month] — Top 10 (win rate)",
				"/leaderboard [week|month] rr — Top 10 (total RR)",
			},
			Run: func(h *Handler, ctx context.Context, msg *Message, text string) { h.cmdLeaderboard(ctx, msg, text) },
		},
		{
			Name: "export", Group: groupExport,
			Desc: "Export jurnal ke CSV/PDF",
			Help: []string{
				"/export — Export journal (CSV)",
				"/export pdf — Export journal (PDF)",
			},
			Run: func(h *Handler, ctx context.Context, msg *Message, text string) { h.cmdExport(ctx, msg, text) },
		},
		{
			Name: "report", Group: groupExport,
			Desc: "Ringkasan mingguan komunitas",
			Help: []string{"/report — Summary report minggu ini"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdReport(ctx, msg) },
		},
		{
			Name: "profile", Group: groupOther,
			Desc: "Profil level, XP & streak",
			Help: []string{"/profile — Profil &amp; level gamifikasi"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdProfile(ctx, msg) },
		},
		{
			Name: "badges", Group: groupOther,
			Desc: "Koleksi badge pencapaian",
			Help: []string{"/badges — Koleksi badge"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdBadges(ctx, msg) },
		},
		{
			Name: "challenge", Group: groupOther,
			Desc: "Weekly challenge & klasemen",
			Help: []string{"/challenge — Weekly challenge &amp; standings"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdChallenge(ctx, msg) },
		},
		{
			Name: "reminder", Group: groupOther,
			Desc: "Atur pengingat jurnal harian",
			Help: []string{"/reminder — Atur daily reminder"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, text string) { h.cmdReminder(ctx, msg, text) },
		},
		{
			Name: "goal", Group: groupOther,
			Desc: "Target bulanan & progress",
			Help: []string{"/goal — Monthly goal &amp; progress"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, text string) { h.cmdGoal(ctx, msg, text) },
		},
		{
			Name: "analyze", Group: groupOther,
			Desc: "Analisis trading dengan AI",
			Help: []string{"/analyze — AI trade analytics"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdAnalyze(ctx, msg) },
		},
		{
			Name: "reportcard", Group: groupOther,
			Desc: "Rapor bulanan trader",
			Help: []string{"/reportcard — Monthly report card"},
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.cmdReportCard(ctx, msg) },
		},
		{
			Name: "help", Group: groupOther,
			Desc: "Panduan penggunaan bot",
			Help: []string{"/help — Tampilkan pesan ini"},
			Run: func(h *Handler, ctx context.Context, msg *Message, _ string) { h.sendHelp(ctx, msg) },
		},
		{
			Name: "start", Group: groupOther,
			Desc: "Mulai bot (sama dengan /help)",
			Help: nil, // alias of /help, kept out of the listing
			Run:  func(h *Handler, ctx context.Context, msg *Message, _ string) { h.sendHelp(ctx, msg) },
		},
		{
			Name: "reset", Group: groupOwner,
			Desc:      "Reset semua data BadgerDB & Notion (Owner Only)",
			Help:      []string{"/reset — Reset semua data BadgerDB &amp; Notion"},
			OwnerOnly: true,
			Run:       func(h *Handler, ctx context.Context, msg *Message, text string) { h.cmdReset(ctx, msg, text) },
		},
	}
}

// commandIndex maps a command name to its catalog entry.
var commandIndex = sync.OnceValue(func() map[string]Command {
	catalog := commandCatalog()
	index := make(map[string]Command, len(catalog))
	for _, cmd := range catalog {
		index[cmd.Name] = cmd
	}
	return index
})

// sendHelp renders the /help message, including owner-only commands for the owner.
func (h *Handler) sendHelp(ctx context.Context, msg *Message) {
	isOwner := msg.From != nil && h.ownerID > 0 && msg.From.ID == h.ownerID
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatHelpForUser(isOwner), msg.MessageThreadID)
}

// parseCommandName extracts the command name from the first token of text.
//
// It returns false for anything that is not a slash command. A "/stats@MyBot"
// suffix (added by Telegram clients in group chats) is stripped, and the name is
// lower-cased so routing never depends on the user's capitalisation.
func parseCommandName(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	head := text
	if i := strings.IndexAny(head, " \t\r\n"); i >= 0 {
		head = head[:i]
	}
	name := strings.ToLower(strings.TrimPrefix(head, "/"))
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return "", false
	}
	return name, true
}

// dispatchCommand routes a slash command to its handler. It reports false when
// the text is not a known command, leaving the caller to try other routes.
func (h *Handler) dispatchCommand(ctx context.Context, msg *Message, text string) bool {
	name, ok := parseCommandName(text)
	if !ok {
		return false
	}
	cmd, ok := commandIndex()[name]
	if !ok {
		return false
	}
	// Owner-only commands are silently ignored for everyone else, so a
	// non-owner cannot even probe for their existence.
	if cmd.OwnerOnly && (msg.From == nil || h.ownerID == 0 || msg.From.ID != h.ownerID) {
		return true
	}
	cmd.Run(h, ctx, msg, text)
	return true
}

// CommandMenus returns the command-menu entries for Telegram: public commands
// for every chat, plus owner-only commands for the owner's private scope.
func CommandMenus() (public, ownerOnly []BotCommand) {
	for _, cmd := range commandCatalog() {
		if cmd.Desc == "" {
			continue // hidden alias
		}
		entry := BotCommand{Command: cmd.Name, Description: cmd.Desc}
		if cmd.OwnerOnly {
			ownerOnly = append(ownerOnly, entry)
			continue
		}
		public = append(public, entry)
	}
	return public, ownerOnly
}

// FormatHelpForUser renders the /help message from commandCatalog.
func FormatHelpForUser(isOwner bool) string {
	var sb strings.Builder
	sb.WriteString("🔐 <b>ARK Vault — Help</b>\n")

	byGroup := make(map[string][]Command, len(helpSections))
	for _, cmd := range commandCatalog() {
		if cmd.OwnerOnly || len(cmd.Help) == 0 {
			continue
		}
		byGroup[cmd.Group] = append(byGroup[cmd.Group], cmd)
	}

	for _, section := range helpSections {
		cmds := byGroup[section.title]
		if len(cmds) == 0 && section.note == "" {
			continue
		}
		fmt.Fprintf(&sb, "\n<b>%s</b>\n", section.title)
		for _, cmd := range cmds {
			for _, line := range cmd.Help {
				fmt.Fprintf(&sb, "• %s\n", line)
			}
		}
		if section.note != "" {
			sb.WriteString(section.note)
		}
	}

	fmt.Fprintf(&sb, "\n%s\n", helpTips)

	if isOwner {
		sb.WriteString("\n<b>" + groupOwner + "</b>\n")
		for _, cmd := range commandCatalog() {
			if !cmd.OwnerOnly {
				continue
			}
			for _, line := range cmd.Help {
				fmt.Fprintf(&sb, "• %s\n", line)
			}
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}
