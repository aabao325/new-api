package costprofit

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupInstallTestDB points both upstream handles at one in-memory SQLite
// database and restores the process globals afterwards, so these tests cannot
// leak state into the rest of the package.
func setupInstallTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	originalDB, originalLogDB := model.DB, model.LOG_DB
	originalMain, originalLog := common.MainDatabaseType(), common.LogDatabaseType()
	originalIsMaster := common.IsMasterNode
	originalState := state.Load()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = originalDB, originalLogDB
		common.SetDatabaseTypes(originalMain, originalLog)
		common.IsMasterNode = originalIsMaster
		state.Store(originalState)
	})

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.IsMasterNode = true

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB, model.LOG_DB = db, db

	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestInstallCreatesSchemaAndIsIdempotent is the automated half of the
// migration check: it proves the hand-written gorm tags on all twelve tables
// actually parse and build, and that a restart is a no-op.
//
// It is not a substitute for the MySQL and PostgreSQL verification AGENTS.md
// requires for schema changes; SQLite accepts index and column shapes the other
// two engines can still reject.
func TestInstallCreatesSchemaAndIsIdempotent(t *testing.T) {
	db := setupInstallTestDB(t)

	Install()
	require.True(t, panelReady(), "install failed: %s", panelFailure())

	for _, table := range append(mainTables(), ledgerTables()...) {
		stmt := &gorm.Statement{DB: db}
		require.NoError(t, stmt.Parse(table))
		assert.True(t, db.Migrator().HasTable(table), "table %s was not created", stmt.Table)
	}

	// The cursor must exist before the first settlement, because the window is
	// derived from it.
	var cursor SettlementCursor
	require.NoError(t, db.Take(&cursor, CursorRowId).Error)
	assert.Zero(t, cursor.SettledBefore, "a fresh install must start with an unset watermark")

	// Running again is what a restart does. It must neither error nor reset the
	// watermark, which would re-settle already-settled logs and double-count.
	require.NoError(t, db.Model(&SettlementCursor{}).Where("id = ?", CursorRowId).
		Update("settled_before", time.Now().Unix()).Error)
	var advanced SettlementCursor
	require.NoError(t, db.Take(&advanced, CursorRowId).Error)

	Install()
	require.True(t, panelReady(), "second install failed: %s", panelFailure())

	var afterRestart SettlementCursor
	require.NoError(t, db.Take(&afterRestart, CursorRowId).Error)
	assert.Equal(t, advanced.SettledBefore, afterRestart.SettledBefore,
		"reinstall rewound the watermark; settled logs would be counted twice")

	var cursorCount int64
	require.NoError(t, db.Model(&SettlementCursor{}).Count(&cursorCount).Error)
	assert.Equal(t, int64(1), cursorCount, "the cursor must stay a singleton")
}

// TestInstallRefusesWithoutDatabase pins the failure contract: a panel that
// cannot install must disable itself and say why, never take the gateway down.
// Install is called from main before the HTTP server starts, so a panic or a
// fatal error here would stop the whole gateway from serving traffic over a
// feature that is entirely additive.
func TestInstallRefusesWithoutDatabase(t *testing.T) {
	setupInstallTestDB(t)
	model.DB = nil

	Install()

	assert.False(t, panelReady(), "install must not report success without a database")
	assert.NotEmpty(t, panelFailure(), "a disabled panel must explain why")
}
