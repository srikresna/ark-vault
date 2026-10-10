package service

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// ChallengeService manages weekly challenges.
type ChallengeService struct {
	store    ChallengeStore
	trades   TradeRepository
	members  MemberRepository
	gamSvc   *GamificationService
	badgeSvc *BadgeService
}

// NewChallengeService creates a new ChallengeService.
func NewChallengeService(
	store ChallengeStore,
	trades TradeRepository,
	members MemberRepository,
	gamSvc *GamificationService,
	badgeSvc *BadgeService,
) *ChallengeService {
	return &ChallengeService{
		store:    store,
		trades:   trades,
		members:  members,
		gamSvc:   gamSvc,
		badgeSvc: badgeSvc,
	}
}

// GetOrCreateChallenge gets or creates the weekly challenge for the given week.
func (s *ChallengeService) GetOrCreateChallenge(ctx context.Context, t time.Time) (*domain.WeeklyChallenge, error) {
	yearWeek := domain.YearWeekString(t)

	existing, err := s.store.GetChallenge(ctx, yearWeek)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	// Rotate through templates based on ISO week number.
	_, week := t.ISOWeek()
	tmpl := domain.ChallengeTemplates[week%len(domain.ChallengeTemplates)]

	// Calculate Monday (start) and Sunday (end) of the ISO week.
	// time.ISOWeek considers Monday as day 1.
	weekday := t.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	monday := t.AddDate(0, 0, -int(weekday-time.Monday))
	monday = time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, wib)
	sunday := monday.AddDate(0, 0, 6)
	sunday = time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 23, 59, 59, 0, wib)

	challenge := &domain.WeeklyChallenge{
		YearWeek:    yearWeek,
		Type:        tmpl.Type,
		Title:       tmpl.Title,
		Description: tmpl.Description,
		StartDate:   monday,
		EndDate:     sunday,
		Finalized:   false,
	}

	if err := s.store.SaveChallenge(ctx, challenge); err != nil {
		return nil, err
	}
	return challenge, nil
}

// GetCurrentStandings computes live standings for the given challenge.
func (s *ChallengeService) GetCurrentStandings(ctx context.Context, challenge *domain.WeeklyChallenge) ([]domain.ChallengeResult, error) {
	members, err := s.members.ListMembers(ctx)
	if err != nil {
		return nil, err
	}

	var results []domain.ChallengeResult

	for _, m := range members {
		trades, err := s.trades.GetTrades(ctx, m.TelegramID)
		if err != nil {
			return nil, err
		}

		// Filter trades within challenge date range.
		var weekTrades []domain.Trade
		for _, t := range trades {
			if !t.Date.Before(challenge.StartDate) && !t.Date.After(challenge.EndDate) {
				weekTrades = append(weekTrades, t)
			}
		}

		if len(weekTrades) == 0 {
			continue
		}

		var value float64
		switch challenge.Type {
		case domain.ChallengeMostTrades:
			value = float64(len(weekTrades))
		case domain.ChallengeBestRR:
			value = math.Inf(-1)
			for _, t := range weekTrades {
				if t.ResultRR > value {
					value = t.ResultRR
				}
			}
		case domain.ChallengeHighestWR:
			if len(weekTrades) < 3 {
				continue // minimum 3 trades required
			}
			var wins int
			for _, t := range weekTrades {
				if t.Status == domain.StatusWin {
					wins++
				}
			}
			value = float64(wins) / float64(len(weekTrades)) * 100
		case domain.ChallengeMostRR:
			for _, t := range weekTrades {
				value += t.ResultRR
			}
		}

		results = append(results, domain.ChallengeResult{
			TelegramID: m.TelegramID,
			Username:   m.Username,
			Value:      value,
		})
	}

	// Sort by value descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Value > results[j].Value
	})

	// Assign ranks.
	for i := range results {
		results[i].Rank = i + 1
	}

	return results, nil
}

// FinalizeChallenge finalizes a challenge: compute final standings, award XP/badges.
func (s *ChallengeService) FinalizeChallenge(ctx context.Context, yearWeek string) ([]domain.ChallengeResult, error) {
	challenge, err := s.store.GetChallenge(ctx, yearWeek)
	if err != nil {
		return nil, err
	}
	if challenge == nil {
		return nil, nil
	}
	if challenge.Finalized {
		return nil, nil
	}

	// Compute final standings.
	results, err := s.GetCurrentStandings(ctx, challenge)
	if err != nil {
		return nil, err
	}

	// Award XP based on placement.
	for _, r := range results {
		var xp int
		var reason string
		switch r.Rank {
		case 1:
			xp = domain.XPChallenge1st
			reason = "challenge_1st"
		case 2:
			xp = domain.XPChallenge2nd
			reason = "challenge_2nd"
		case 3:
			xp = domain.XPChallenge3rd
			reason = "challenge_3rd"
		default:
			xp = domain.XPChallengeParticip
			reason = "challenge_participation"
		}

		if _, err := s.gamSvc.AwardXP(ctx, r.TelegramID, xp, reason); err != nil {
			return nil, err
		}
	}

	// Award challenge_winner badge to 1st place.
	if len(results) > 0 {
		if _, err := s.badgeSvc.AwardChallengeBadge(ctx, results[0].TelegramID); err != nil {
			return nil, err
		}
	}

	// Mark as finalized.
	challenge.Finalized = true
	if err := s.store.SaveChallenge(ctx, challenge); err != nil {
		return nil, err
	}

	return results, nil
}
