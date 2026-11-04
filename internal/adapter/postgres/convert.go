package postgres

import (
	"strconv"
)

// parseInt64 converts a decimal string id back into an int64. It exists because
// members are addressed by their Telegram ID (int64) while trades carry the
// surrogate member uuid, and a join that returned the uuid as text must not
// silently leave MemberID at zero.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
