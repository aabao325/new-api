package costprofit

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

// LedgerDSNEnv optionally points the profit ledger at its own database.
//
// Empty (the default) keeps the ledger beside the logs, which needs no new
// infrastructure. Setting it decouples lifecycles: the logs table is
// disposable (it has a TTL, a cleanup task, and can be rebuilt), while profit
// records are financial data that must be kept, so wiping or rebuilding the log
// database should not force anyone to rescue these tables first. The value may
// point at another database inside the same server, so isolation does not
// require another container.
const LedgerDSNEnv = "COST_PROFIT_SQL_DSN"

// ledgerSQLiteFile is used when LedgerDSNEnv asks for SQLite explicitly.
const ledgerSQLiteFile = "cost-profit.db"

// runtime is the installed state, published as one immutable value so readers
// never observe a half-initialized panel.
type runtime struct {
	main       *gorm.DB
	ledger     *gorm.DB
	ledgerType common.DatabaseType
	// separate is true when the ledger has its own connection rather than
	// sharing the log database handle.
	separate bool
	ready    bool
	// failure explains why the panel is unavailable, for the status endpoint.
	failure string
}

var state atomic.Pointer[runtime]

func current() *runtime {
	if s := state.Load(); s != nil {
		return s
	}
	return &runtime{failure: "cost profit panel is not installed"}
}

// panelReady reports whether the tables exist and the handles are usable.
func panelReady() bool { return current().ready }

// panelFailure is the reason the panel is unavailable, or "" when it is fine.
func panelFailure() string { return current().failure }

func mainDB() *gorm.DB { return current().main }

func ledgerDB() *gorm.DB { return current().ledger }

func ledgerDatabaseType() common.DatabaseType {
	if s := state.Load(); s != nil && s.ledgerType != "" {
		return s.ledgerType
	}
	return common.LogDatabaseType()
}

// Install creates this package's tables and resolves its database handles. It
// is the panel's single entry point into startup, called once from main after
// the log database is ready.
//
// It never returns an error and never panics: a broken panel must not take the
// gateway down with it, because the panel is an addition to upstream rather
// than part of it. Failures are logged and reported by the status endpoint,
// and every route refuses to serve until Install succeeds.
func Install() {
	if model.DB == nil {
		disable("main database is not initialized")
		return
	}

	ledger, ledgerType, separate, err := openLedger()
	if err != nil {
		disable(err.Error())
		return
	}

	// A ClickHouse ledger cannot work: MergeTree has no unique constraint to
	// enforce uk_pl_bucket and no transaction to make "delete the window,
	// reinsert it, advance the cursor" atomic, so settlement would double-count
	// instead of being idempotent. Fall back to the main database, which is
	// always one of the three transactional engines.
	if ledgerType == common.DatabaseTypeClickHouse {
		common.SysError("cost profit: log database is ClickHouse, which supports neither unique constraints nor transactions; " +
			"keeping the profit ledger in the main database instead. Set " + LedgerDSNEnv + " to choose another location.")
		ledger, ledgerType, separate = model.DB, common.MainDatabaseType(), false
	}

	rt := &runtime{main: model.DB, ledger: ledger, ledgerType: ledgerType, separate: separate}

	// Follow the upstream convention: only the master node migrates, so slave
	// nodes cannot race it with conflicting DDL.
	if common.IsMasterNode {
		if err := migrate(rt); err != nil {
			disable("migration failed: " + err.Error())
			return
		}
	}

	rt.ready = true
	state.Store(rt)
	location := "shared with logs"
	if separate {
		location = "dedicated via " + LedgerDSNEnv
	}
	common.SysLog(fmt.Sprintf("cost profit panel installed (ledger: %s, %s)", ledgerType, location))
}

// disable records why the panel cannot serve and logs it once at startup.
func disable(reason string) {
	state.Store(&runtime{failure: reason})
	common.SysError("cost profit panel disabled: " + reason)
}

// openLedger resolves where the profit ledger lives. With LedgerDSNEnv unset it
// shares model.LOG_DB, so the default deployment needs no new configuration.
func openLedger() (db *gorm.DB, dbType common.DatabaseType, separate bool, err error) {
	dsn := strings.TrimSpace(os.Getenv(LedgerDSNEnv))
	if dsn == "" {
		if model.LOG_DB == nil {
			return nil, "", false, fmt.Errorf("log database is not initialized")
		}
		return model.LOG_DB, common.LogDatabaseType(), false, nil
	}

	// model.chooseDB and model.newGormConfig are unexported, so this reproduces
	// the parts that matter for a transactional ledger. ClickHouse is absent on
	// purpose: it cannot satisfy the settlement transaction.
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		// PreferSimpleProtocol mirrors model.chooseDB: named prepared statements
		// break against transaction-pooling proxies (PgBouncer/Neon/Supabase)
		// with FATAL 08P01/42P05.
		db, err = gorm.Open(postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true}), ledgerGormConfig(false))
		dbType = common.DatabaseTypePostgreSQL
	case strings.HasPrefix(dsn, "local"):
		// Reuse upstream's pragma set (WAL, 30s busy timeout, BEGIN IMMEDIATE);
		// the reasons are documented on common.SQLitePath.
		path := ledgerSQLiteFile
		if _, pragmas, ok := strings.Cut(common.SQLitePath, "?"); ok {
			path += "?" + pragmas
		}
		db, err = gorm.Open(sqlite.Open(path), ledgerGormConfig(true))
		dbType = common.DatabaseTypeSQLite
	default:
		if !strings.Contains(dsn, "parseTime") {
			if strings.Contains(dsn, "?") {
				dsn += "&parseTime=true"
			} else {
				dsn += "?parseTime=true"
			}
		}
		db, err = gorm.Open(mysql.New(mysql.Config{DSN: dsn}), ledgerGormConfig(true))
		dbType = common.DatabaseTypeMySQL
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("cannot open %s: %w", LedgerDSNEnv, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, "", false, fmt.Errorf("cannot reach %s: %w", LedgerDSNEnv, err)
	}
	sqlDB.SetMaxIdleConns(common.GetEnvOrDefault("SQL_MAX_IDLE_CONNS", 100))
	sqlDB.SetMaxOpenConns(common.GetEnvOrDefault("SQL_MAX_OPEN_CONNS", 1000))
	sqlDB.SetConnMaxLifetime(time.Second * time.Duration(common.GetEnvOrDefault("SQL_MAX_LIFETIME", 60)))
	return db, dbType, true, nil
}

// ledgerGormConfig configures the dedicated ledger connection. Upstream's
// sanitizing log writer is not reused because it is unexported and these tables
// hold no credentials, only ids and quota amounts.
func ledgerGormConfig(prepareStmt bool) *gorm.Config {
	return &gorm.Config{
		PrepareStmt: prepareStmt,
		Logger:      gormlogger.Default.LogMode(gormlogger.Warn),
	}
}

// migrate creates or updates this package's tables. AutoMigrate is additive and
// idempotent, so a restart is a no-op and an upgrade only adds what is missing.
func migrate(rt *runtime) error {
	if err := rt.main.AutoMigrate(mainTables()...); err != nil {
		return fmt.Errorf("main tables: %w", err)
	}
	if err := rt.ledger.AutoMigrate(ledgerTables()...); err != nil {
		return fmt.Errorf("ledger tables: %w", err)
	}
	return ensureCursor(rt.ledger)
}

// ensureCursor creates the singleton watermark row. A settlement window is
// derived from it, so it must exist before the first run; DoNothing keeps two
// master nodes starting together from colliding on the primary key.
func ensureCursor(db *gorm.DB) error {
	cursor := SettlementCursor{Id: CursorRowId, SettledBefore: 0, UpdatedAt: time.Now().Unix()}
	err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&cursor).Error
	if err != nil {
		return fmt.Errorf("settlement cursor: %w", err)
	}
	return nil
}
