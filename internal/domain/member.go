package domain

import "time"

// Role controls what a member may do in the web app. Telegram has no roles, so
// a member's role is assigned here and defaults to RoleMember.
type Role string

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// CanManageOthers reports whether the role may read and act on other members.
func (r Role) CanManageOthers() bool {
	return r == RoleAdmin || r == RoleOwner
}

// Valid reports whether the role is one of the known values.
func (r Role) Valid() bool {
	switch r {
	case RoleMember, RoleAdmin, RoleOwner:
		return true
	default:
		return false
	}
}

// Member represents a community member tracked by the bot and the web app.
type Member struct {
	// ID is the surrogate primary key. TelegramID stays the business key the
	// bot speaks; ID exists so relational rows and web sessions never depend on
	// an identifier owned by an external platform.
	ID string
	// TelegramID is the identity every member starts with: membership is
	// granted through the Telegram community group.
	TelegramID int64
	// DiscordID is an optional second login identity linked to the same member.
	// Empty when the member has not connected Discord.
	DiscordID string
	Username  string
	FirstName string
	Role      Role
	JoinDate  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}
