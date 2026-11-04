package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// ChallengeRepo stores the rotating weekly challenge.
type ChallengeRepo struct {
	db *DB
}

// NewChallengeRepo builds the challenge repository.
func NewChallengeRepo(db *DB) *ChallengeRepo { return &ChallengeRepo{db: db} }

// GetChallenge returns the challenge for a year-week, or nil when none exists.
func (r *ChallengeRepo) GetChallenge(ctx context.Context, yearWeek string) (*domain.WeeklyChallenge, error) {
	var c domain.WeeklyChallenge
	err := r.db.pool.QueryRow(ctx, `
		SELECT year_week, kind, title, description, start_date, end_date, finalized
		  FROM challenges WHERE year_week = $1`, yearWeek,
	).Scan(&c.YearWeek, &c.Type, &c.Title, &c.Description, &c.StartDate, &c.EndDate, &c.Finalized)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get challenge %s: %w", yearWeek, err)
	}
	return &c, nil
}

// SaveChallenge upserts the challenge.
func (r *ChallengeRepo) SaveChallenge(ctx context.Context, challenge *domain.WeeklyChallenge) error {
	if challenge == nil || challenge.YearWeek == "" {
		return errors.New("save challenge: year week is required")
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO challenges (year_week, kind, title, description,
		                        start_date, end_date, finalized)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (year_week) DO UPDATE
		   SET kind = EXCLUDED.kind, title = EXCLUDED.title,
		       description = EXCLUDED.description,
		       start_date = EXCLUDED.start_date, end_date = EXCLUDED.end_date,
		       finalized = EXCLUDED.finalized`,
		challenge.YearWeek, string(challenge.Type), challenge.Title,
		challenge.Description, challenge.StartDate, challenge.EndDate,
		challenge.Finalized); err != nil {
		return fmt.Errorf("save challenge %s: %w", challenge.YearWeek, err)
	}
	return nil
}

// GoalRepo stores monthly goals.
type GoalRepo struct {
	db      *DB
	members *MemberRepo
}

// NewGoalRepo builds the goal repository.
func NewGoalRepo(db *DB, members *MemberRepo) *GoalRepo {
	return &GoalRepo{db: db, members: members}
}

// GetGoal returns a member's goal for a month, or nil when none exists.
func (r *GoalRepo) GetGoal(ctx context.Context, telegramID int64, yearMonth string) (*domain.MonthlyGoal, error) {
	var g domain.MonthlyGoal
	var achievedAt *time.Time
	err := r.db.pool.QueryRow(ctx, `
		SELECT m.telegram_id, g.year_month, g.goal_type, g.target_value,
		       g.achieved, g.achieved_at, g.created_at
		  FROM goals g
		  JOIN members m ON m.id = g.member_id
		 WHERE m.telegram_id = $1 AND g.year_month = $2`, telegramID, yearMonth,
	).Scan(&g.TelegramID, &g.YearMonth, &g.GoalType, &g.TargetValue,
		&g.Achieved, &achievedAt, &g.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get goal %d/%s: %w", telegramID, yearMonth, err)
	}
	if achievedAt != nil {
		g.AchievedAt = *achievedAt
	}
	return &g, nil
}

// SaveGoal upserts the goal.
func (r *GoalRepo) SaveGoal(ctx context.Context, goal *domain.MonthlyGoal) error {
	if goal == nil || goal.YearMonth == "" {
		return errors.New("save goal: year month is required")
	}
	mid, err := r.members.memberID(ctx, goal.TelegramID)
	if err != nil {
		return err
	}
	var achievedAt any
	if !goal.AchievedAt.IsZero() {
		achievedAt = goal.AchievedAt
	}
	created := goal.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO goals (member_id, year_month, goal_type, target_value,
		                   achieved, achieved_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (member_id, year_month) DO UPDATE
		   SET goal_type = EXCLUDED.goal_type,
		       target_value = EXCLUDED.target_value,
		       achieved = EXCLUDED.achieved,
		       achieved_at = EXCLUDED.achieved_at`,
		mid, goal.YearMonth, string(goal.GoalType), goal.TargetValue,
		goal.Achieved, achievedAt, created); err != nil {
		return fmt.Errorf("save goal %d/%s: %w", goal.TelegramID, goal.YearMonth, err)
	}
	return nil
}

// ListActiveGoals returns every goal for a month that has not been achieved.
func (r *GoalRepo) ListActiveGoals(ctx context.Context, yearMonth string) ([]domain.MonthlyGoal, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT m.telegram_id, g.year_month, g.goal_type, g.target_value,
		       g.achieved, g.achieved_at, g.created_at
		  FROM goals g
		  JOIN members m ON m.id = g.member_id
		 WHERE g.year_month = $1 AND NOT g.achieved
		 ORDER BY m.telegram_id`, yearMonth)
	if err != nil {
		return nil, fmt.Errorf("list active goals %s: %w", yearMonth, err)
	}
	defer rows.Close()

	var out []domain.MonthlyGoal
	for rows.Next() {
		var g domain.MonthlyGoal
		var achievedAt *time.Time
		if err := rows.Scan(&g.TelegramID, &g.YearMonth, &g.GoalType,
			&g.TargetValue, &g.Achieved, &achievedAt, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("list active goals %s: %w", yearMonth, err)
		}
		if achievedAt != nil {
			g.AchievedAt = *achievedAt
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active goals %s: %w", yearMonth, err)
	}
	return out, nil
}
