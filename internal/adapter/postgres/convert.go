package postgres

import (
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// parseInt64 converts a decimal string id back into an int64. It exists because
// members are addressed by their Telegram ID (int64) while a join returns the
// surrogate member uuid as text, and a conversion failure must not silently
// leave MemberID at zero.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// isInvalidUUID reports whether err is Postgres rejecting a string that is not a
// uuid (SQLSTATE 22P02).
//
// Callers turn this into "not found" instead of a 500: a client asking for a
// trade by a malformed id is asking for something that cannot exist, and a 500
// would report an internal error for a plain bad request.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "22P02"
}
