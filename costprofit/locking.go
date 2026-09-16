package costprofit

import (
	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockForUpdate makes the next query emit SELECT ... FOR UPDATE so the matched
// rows stay locked until the surrounding transaction ends.
//
// This duplicates model.lockForUpdate, which is unexported. Importing it would
// mean adding an exported helper to the model package, and every upstream file
// this package touches is a future merge conflict; five lines of duplication
// buys a package boundary that upstream never has to know about.
//
// GORM v2 silently ignores the legacy `Set("gorm:query_option", "FOR UPDATE")`
// from GORM v1, so that form does not lock anything. Always use this helper.
//
// dbType selects the dialect because this package locks rows in two different
// databases: the settlement cursor lives beside the ledger, while payout rows
// live in the main database, and the two can be different engines.
func lockForUpdate(tx *gorm.DB, dbType common.DatabaseType) *gorm.DB {
	// SQLite has no FOR UPDATE syntax (the clause would be a syntax error), and
	// ClickHouse has neither row locks nor transactions. In both cases the
	// caller's serialization comes from elsewhere: SQLite's single-writer model
	// fails one of two conflicting transactions, and a ClickHouse ledger is
	// already refused at install time.
	switch dbType {
	case common.DatabaseTypeSQLite, common.DatabaseTypeClickHouse:
		return tx
	default:
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
}

// lockMainForUpdate locks rows in the main database (payouts, cost versions,
// attribution).
func lockMainForUpdate(tx *gorm.DB) *gorm.DB {
	return lockForUpdate(tx, common.MainDatabaseType())
}

// lockLedgerForUpdate locks rows in the ledger database (the settlement cursor).
func lockLedgerForUpdate(tx *gorm.DB) *gorm.DB {
	return lockForUpdate(tx, ledgerDatabaseType())
}
