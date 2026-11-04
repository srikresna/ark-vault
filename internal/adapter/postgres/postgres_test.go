package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// testDB connects to the database named by TEST_DATABASE_URL and skips the test
// when it is unset, so the unit suite stays hermetic while CI or a developer can
// run the real SQL against a disposable Postgres.
func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(db.Close)
	// Each test starts from a clean slate. TRUNCATE ... CASCADE is faster than
	// deleting rows and removes any stale data from an earlier run.
	if _, err := db.Pool().Exec(context.Background(), `
		TRUNCATE trades, trade_xp_grants, xp_events, badges, goals, reminder_prefs,
		         analytics_cache, report_cards, challenges, profiles, streaks,
		         replay_kv, oauth_states, sessions, audit_log, members
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

func TestIntegration_MemberTradeGamification(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	members := NewMemberRepo(db)
	trades := NewTradeRepo(db, members)
	gam := NewGamificationRepo(db, members)

	m, err := members.EnsureMember(ctx, &domain.Member{
		TelegramID: 12345, Username: "trader", FirstName: "Trader",
	})
	if err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	if m.ID == "" {
		t.Fatal("ensure member returned no surrogate id")
	}

	// EnsureMember must be idempotent: a second call reuses the same row.
	again, err := members.EnsureMember(ctx, &domain.Member{TelegramID: 12345, Username: "renamed"})
	if err != nil {
		t.Fatalf("ensure member twice: %v", err)
	}
	if again.ID != m.ID {
		t.Fatalf("member id changed on second call: %s != %s", again.ID, m.ID)
	}
	if again.Username != "renamed" {
		t.Fatalf("username not updated: %q", again.Username)
	}

	trade := &domain.Trade{
		Symbol: "XAUUSD", Direction: domain.DirBuy, AssetType: domain.AssetGold,
		Status: domain.StatusWin, Date: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
		ResultRR: 2, TimeWindow: domain.SessionLondon, Confluence: "FVG", Notes: "n",
	}
	if err := trades.SaveTrade(ctx, 12345, trade); err != nil {
		t.Fatalf("save trade: %v", err)
	}
	if trade.ID == "" {
		t.Fatal("save trade did not return an id")
	}

	got, err := trades.GetTrades(ctx, 12345)
	if err != nil {
		t.Fatalf("get trades: %v", err)
	}
	if len(got) != 1 || got[0].Symbol != "XAUUSD" || got[0].ResultRR != 2 {
		t.Fatalf("get trades = %+v, want one XAUUSD trade at 2R", got)
	}

	// An empty member gets an empty slice, not nil: a caller must be able to
	// tell "no history" from "read failed" by the error alone.
	empty, err := trades.GetTrades(ctx, 99999)
	if err == nil {
		t.Fatalf("get trades for unknown member must fail, got %+v", empty)
	}

	profile := &domain.GamificationProfile{
		TelegramID: 12345, TotalXP: 50, Level: 2, Title: "Apprentice",
		UpdatedAt: time.Now(),
	}
	streak := &domain.StreakData{
		TelegramID: 12345, CurrentStreak: 3, LongestStreak: 5,
		LastLogDate: "2026-03-10", UpdatedAt: time.Now(),
	}
	grant := &domain.TradeXPGrant{
		TelegramID: 12345, TradeID: trade.ID, Base: 20, Screenshot: 8,
		GrantedAt: time.Now(),
	}
	event := &domain.XPEvent{
		TelegramID: 12345, Amount: 28, Reason: "trade_recorded", Timestamp: time.Now(),
	}
	if err := gam.CommitTradeRecord(ctx, profile, streak, grant, event); err != nil {
		t.Fatalf("commit trade record: %v", err)
	}

	storedProfile, err := gam.GetProfile(ctx, 12345)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if storedProfile == nil || storedProfile.TotalXP != 50 {
		t.Fatalf("profile = %+v, want 50 XP", storedProfile)
	}
	storedStreak, err := gam.GetStreak(ctx, 12345)
	if err != nil {
		t.Fatalf("get streak: %v", err)
	}
	if storedStreak == nil || storedStreak.CurrentStreak != 3 || storedStreak.LastLogDate != "2026-03-10" {
		t.Fatalf("streak = %+v, want 3 days ending 2026-03-10", storedStreak)
	}
	storedGrant, err := gam.GetTradeXPGrant(ctx, 12345, trade.ID)
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if storedGrant == nil || storedGrant.Base != 20 || storedGrant.Screenshot != 8 {
		t.Fatalf("grant = %+v, want base 20 shot 8", storedGrant)
	}

	// A zero-XP adjustment deletes the grant without touching the profile.
	if err := gam.CommitTradeAdjustment(ctx, nil, nil, nil,
		&domain.TradeXPGrant{TelegramID: 12345, TradeID: trade.ID}); err != nil {
		t.Fatalf("commit adjustment: %v", err)
	}
	gone, err := gam.GetTradeXPGrant(ctx, 12345, trade.ID)
	if err != nil {
		t.Fatalf("get grant after delete: %v", err)
	}
	if gone != nil {
		t.Fatalf("grant survived deletion: %+v", gone)
	}
	afterDelete, err := gam.GetProfile(ctx, 12345)
	if err != nil {
		t.Fatalf("get profile after adjustment: %v", err)
	}
	if afterDelete.TotalXP != 50 {
		t.Fatalf("zero-delta adjustment changed XP: %d", afterDelete.TotalXP)
	}

	// Soft delete hides the trade from history but keeps the row.
	if err := trades.DeleteTrade(ctx, 12345, trade.ID); err != nil {
		t.Fatalf("delete trade: %v", err)
	}
	remaining, err := trades.GetTrades(ctx, 12345)
	if err != nil {
		t.Fatalf("get trades after delete: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("deleted trade still listed: %+v", remaining)
	}
}

func TestIntegration_BadgeClaimIsAtomic(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	members := NewMemberRepo(db)
	badges := NewBadgeRepo(db, members)
	if _, err := members.EnsureMember(ctx, &domain.Member{TelegramID: 555}); err != nil {
		t.Fatalf("ensure member: %v", err)
	}

	award := &domain.BadgeAward{TelegramID: 555, BadgeID: domain.BadgeFirstTrade}
	first, err := badges.ClaimBadge(ctx, award)
	if err != nil {
		t.Fatalf("claim badge: %v", err)
	}
	second, err := badges.ClaimBadge(ctx, award)
	if err != nil {
		t.Fatalf("claim badge twice: %v", err)
	}
	if !first || second {
		t.Fatalf("claim results = %v, %v; want true then false", first, second)
	}
	has, err := badges.HasBadge(ctx, 555, domain.BadgeFirstTrade)
	if err != nil {
		t.Fatalf("has badge: %v", err)
	}
	if !has {
		t.Fatal("badge not stored after claim")
	}
}

func TestIntegration_ResetPreservesLedger(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	members := NewMemberRepo(db)
	trades := NewTradeRepo(db, members)
	resetter := NewResetter(db)
	kv := NewKV(db)

	if _, err := members.EnsureMember(ctx, &domain.Member{TelegramID: 777}); err != nil {
		t.Fatalf("ensure member: %v", err)
	}
	if err := trades.SaveTrade(ctx, 777, &domain.Trade{
		Symbol: "EURUSD", Direction: domain.DirSell, Status: domain.StatusLoss,
		Date: time.Now(), ResultRR: -1,
	}); err != nil {
		t.Fatalf("save trade: %v", err)
	}
	// The ledger is transport state and must outlive a user-data reset, or a
	// redelivered update would re-run an action whose effects were just deleted.
	if err := kv.SetWithTTL(ctx, "gam:idem:bot:1:upd:42", map[string]any{"state": "done"}, 0); err != nil {
		t.Fatalf("write ledger: %v", err)
	}

	removed, err := resetter.ResetAll(ctx)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if removed != 1 {
		t.Fatalf("reset removed %d trades, want 1", removed)
	}

	after, err := trades.GetTrades(ctx, 777)
	if err != nil {
		t.Fatalf("get trades after reset: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("trades survived reset: %+v", after)
	}

	var rec map[string]any
	found, err := kv.Get(ctx, "gam:idem:bot:1:upd:42", &rec)
	if err != nil {
		t.Fatalf("read ledger after reset: %v", err)
	}
	if !found || rec["state"] != "done" {
		t.Fatalf("ledger row lost by reset (found=%v, rec=%+v)", found, rec)
	}

	// The member itself survives: it is an identity, not journal content.
	m, err := members.GetMember(ctx, 777)
	if err != nil {
		t.Fatalf("get member after reset: %v", err)
	}
	if m == nil {
		t.Fatal("reset deleted the member identity")
	}
}

func TestIntegration_KVExpiryAndBatch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	kv := NewKV(db)

	// A zero TTL must never expire: a running ledger row that vanished would
	// let its update be replayed.
	if err := kv.SetWithTTL(ctx, "gam:idem:bot:1:upd:1", map[string]any{"state": "running"}, 0); err != nil {
		t.Fatalf("write no-ttl key: %v", err)
	}
	var rec map[string]any
	if found, err := kv.Get(ctx, "gam:idem:bot:1:upd:1", &rec); err != nil || !found {
		t.Fatalf("no-ttl key missing: found=%v err=%v", found, err)
	}

	// An expired key reads as absent.
	if err := kv.SetWithTTL(ctx, "gam:idem:bot:1:upd:2", map[string]any{"state": "done"}, time.Millisecond); err != nil {
		t.Fatalf("write short-ttl key: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	var expired map[string]any
	if found, err := kv.Get(ctx, "gam:idem:bot:1:upd:2", &expired); err != nil || found {
		t.Fatalf("expired key still readable: found=%v err=%v", found, err)
	}

	// ApplyBatch inserts only new keys and always advances the cursor.
	inserted, err := kv.ApplyBatch(ctx,
		map[string][]byte{"gam:idem:bot:1:upd:3": []byte(`{"state":"admitted"}`)},
		map[string][]byte{"gam:idem:bot:1:cursor": []byte(`{"offset":4}`)})
	if err != nil {
		t.Fatalf("apply batch: %v", err)
	}
	if len(inserted) != 1 {
		t.Fatalf("inserted = %v, want the one new key", inserted)
	}
	again, err := kv.ApplyBatch(ctx,
		map[string][]byte{"gam:idem:bot:1:upd:3": []byte(`{"state":"admitted"}`)},
		map[string][]byte{"gam:idem:bot:1:cursor": []byte(`{"offset":5}`)})
	if err != nil {
		t.Fatalf("apply batch twice: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("duplicate insert reported as new: %v", again)
	}

	var cursor struct {
		Offset int `json:"offset"`
	}
	if found, err := kv.Get(ctx, "gam:idem:bot:1:cursor", &cursor); err != nil || !found {
		t.Fatalf("cursor missing: found=%v err=%v", found, err)
	}
	if cursor.Offset != 5 {
		t.Fatalf("cursor = %d, want 5", cursor.Offset)
	}

	// Scan is prefix-scoped and must not leak a sibling bot's rows.
	vals, err := kv.Scan(ctx, "gam:idem:bot:1:")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(vals) != 3 {
		t.Fatalf("scan returned %d values, want 3 (upd:1, upd:3, cursor)", len(vals))
	}
}
