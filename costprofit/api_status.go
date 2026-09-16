package costprofit

import (
	"database/sql"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// StatusResponse backs the banner pinned to the top of the panel.
//
// The watermark fields exist because of a failure mode this package cannot
// prevent from inside: upstream's log cleanup task deletes rows from the logs
// table without consulting this package, so if cleanup runs while the watermark
// is still behind, the profit in that gap is gone for good. Showing the
// watermark next to the oldest surviving log makes the gap visible before it
// costs anything, which is why it is a permanent part of the panel rather than
// a diagnostic page.
type StatusResponse struct {
	Installed bool   `json:"installed"`
	Failure   string `json:"failure,omitempty"`
	LedgerDB  string `json:"ledger_db"`
	// LedgerDedicated reports whether the ledger has its own DSN rather than
	// sharing the log database.
	LedgerDedicated bool `json:"ledger_dedicated"`

	// SettledBefore is the watermark: every log older than this is settled.
	SettledBefore int64 `json:"settled_before"`
	// EarliestLogAt is the oldest surviving log this panel accounts for.
	EarliestLogAt int64 `json:"earliest_log_at"`
	// CleanupGap is true when logs were deleted before being settled, which
	// means unrecoverable profit. It is the one alarm on this endpoint.
	CleanupGap bool `json:"cleanup_gap"`
}

// GetStatus reports whether the panel is usable and how far settlement has run.
// Read-only and available to admins, since managers need the same watermark
// warning as root.
func GetStatus(c *gin.Context) {
	rt := current()
	resp := StatusResponse{
		Installed:       rt.ready,
		Failure:         rt.failure,
		LedgerDB:        string(rt.ledgerType),
		LedgerDedicated: rt.separate,
	}
	if !rt.ready {
		common.ApiSuccess(c, resp)
		return
	}

	ctx := c.Request.Context()
	var cursor SettlementCursor
	if err := rt.ledger.WithContext(ctx).Take(&cursor, CursorRowId).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	resp.SettledBefore = cursor.SettledBefore

	// The logs table belongs to the log database, which is not necessarily where
	// the ledger lives once LedgerDSNEnv is set. Read it through model.LOG_DB.
	//
	// Only the log types this panel settles are considered: an older row of
	// another type is not a gap, so scoping the MIN keeps it comparable to the
	// watermark.
	var earliest struct {
		At sql.NullInt64 `gorm:"column:at"`
	}
	err := model.LOG_DB.WithContext(ctx).Table("logs").
		Where("type IN ?", settledLogTypes()).
		Select("MIN(created_at) AS at").
		Scan(&earliest).Error
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if earliest.At.Valid {
		resp.EarliestLogAt = earliest.At.Int64
		// A watermark of 0 means settlement has never run, so there is nothing
		// it could have missed yet.
		resp.CleanupGap = cursor.SettledBefore > 0 && earliest.At.Int64 > cursor.SettledBefore
	}

	common.ApiSuccess(c, resp)
}
