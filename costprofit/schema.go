// Package costprofit implements an independent channel cost and profit
// management panel.
//
// Design constraint: this package MUST stay mergeable with upstream. It owns
// its own tables and migrations, touches upstream files in exactly two places
// (one line each in main.go and router/api-router.go), and reads upstream data
// (channels, users, logs) read-only. Never add writes to upstream tables here.
package costprofit

// Cost type discriminators for ChannelCost.CostType.
const (
	CostTypeRatio      = "ratio"       // cost = baseQuota * costRatio
	CostTypePerRequest = "per_request" // cost = requestCount * unitPrice * QuotaPerUnit
	CostTypePerSecond  = "per_second"  // cost = seconds * unitPrice * QuotaPerUnit
)

// Attribution sources for ManagerCustomer.Source.
const (
	SourceRootAssign = "root_assign"
	SourceClaim      = "claim"
	SourceInvite     = "invite"
)

// Claim workflow states.
const (
	ClaimPending  = "pending"
	ClaimApproved = "approved"
	ClaimRejected = "rejected"
)

// Cost config provenance for ChannelCost.Source.
const (
	CostSourceManual     = "manual"
	CostSourceNameParsed = "name_parsed"
)

// Settlement run states.
const (
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// settledLogTypes are the upstream log types this panel accounts for: a
// consumption charges the customer, a refund gives it back. Every other type
// (top-up, manage, system, error) moves no billable quota through a channel and
// so has no cost or profit attached.
//
// Values are asserted against model.LogTypeConsume / model.LogTypeRefund in
// upstream_contract_test.go rather than referenced directly, so this package
// keeps its own vocabulary and the test fails loudly if upstream renumbers.
func settledLogTypes() []int { return []int{2, 6} }

// Scale factors for the integer-encoded rational numbers. Ratios and unit
// prices are stored as integers so the schema carries no floating-point
// columns: a float column would require the decimal migration workarounds in
// model/migration_dialector.go (MySQL default-value churn) and the hand-written
// SQLite DDL in model/main.go.
const (
	RatioScale     = 1000    // 0.8 ratio  -> 800
	UnitPriceScale = 1000000 // $0.08/unit -> 80000
)

// OpenInterval marks a still-effective time range. Zero is used rather than
// NULL because NULL values compare unequal to each other in a unique index, so
// a NULL sentinel would not actually constrain "at most one open interval".
const OpenInterval int64 = 0

// ---------------------------------------------------------------------------
// Main database: cost and configuration
// ---------------------------------------------------------------------------

// ChannelCost is an append-only cost version for one channel, optionally
// narrowed to a single model. ModelName == "" is the channel-wide default;
// lookup prefers an exact model match and falls back to that default.
//
// Rows are never deleted, not even when the upstream channel is removed, so
// historical settlement stays reproducible.
type ChannelCost struct {
	Id            int64  `json:"id" gorm:"primaryKey"`
	ChannelId     int    `json:"channel_id" gorm:"not null;index:idx_cc_lookup,priority:1"`
	ModelName     string `json:"model_name" gorm:"type:varchar(128);not null;default:'';index:idx_cc_lookup,priority:2"`
	CostType      string `json:"cost_type" gorm:"type:varchar(16);not null"`
	CostRatioMill int64  `json:"cost_ratio_mill" gorm:"bigint;not null;default:0"`
	UnitPriceMicr int64  `json:"unit_price_micr" gorm:"bigint;not null;default:0"`
	EffectiveFrom int64  `json:"effective_from" gorm:"bigint;not null;index:idx_cc_lookup,priority:3"`
	Source        string `json:"source" gorm:"type:varchar(16);not null;default:''"`
	Note          string `json:"note" gorm:"type:varchar(255);not null;default:''"`
	CreatedBy     int    `json:"created_by" gorm:"not null;default:0"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint;index"`
}

func (ChannelCost) TableName() string { return "cp_channel_cost" }

// ChannelSnapshot remembers what a channel was called and whether it was
// enabled, so the panel can still label historical rows after the channel is
// renamed or deleted upstream. Status 0 means the channel no longer exists.
type ChannelSnapshot struct {
	ChannelId int    `json:"channel_id" gorm:"primaryKey"`
	Name      string `json:"name" gorm:"type:varchar(255);not null;default:''"`
	Status    int    `json:"status" gorm:"not null;default:0"`
	ChannelTp int    `json:"channel_type" gorm:"not null;default:0"`
	SeenAt    int64  `json:"seen_at" gorm:"bigint;not null"`
}

func (ChannelSnapshot) TableName() string { return "cp_channel_snapshot" }

// GlobalConfig is an append-only snapshot of everything except channel costs
// that a settlement needs: group ratios (used to reverse out the pre-discount
// base quota when a log is missing its own group_ratio), the default share
// rate, and per-manager share overrides.
//
// Snapshot holds JSON rather than typed columns so adding a knob later needs no
// migration. It is TEXT, not a native JSON column, because JSON column
// semantics differ across the supported engines and this value is never queried
// from SQL.
type GlobalConfig struct {
	Id            int64  `json:"id" gorm:"primaryKey"`
	EffectiveFrom int64  `json:"effective_from" gorm:"bigint;not null;uniqueIndex:uk_gc_from"`
	Snapshot      string `json:"snapshot" gorm:"type:text;not null"`
	Note          string `json:"note" gorm:"type:varchar(255);not null;default:''"`
	CreatedBy     int    `json:"created_by" gorm:"not null;default:0"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint;index"`
}

func (GlobalConfig) TableName() string { return "cp_global_config" }

// ConfigSnapshot is the decoded form of GlobalConfig.Snapshot.
type ConfigSnapshot struct {
	GroupRatios       map[string]float64 `json:"group_ratios"`
	DefaultShareMill  int64              `json:"default_share_mill"`
	ManagerSharesMill map[string]int64   `json:"manager_shares_mill"`
}

// ---------------------------------------------------------------------------
// Main database: attribution, top-up ratio, settlement records
// ---------------------------------------------------------------------------

// ManagerCustomer attributes one customer to one manager over a time range.
//
// The unique index on (user_id, effective_to) allows at most one open interval
// per customer, which is portable across all three engines and needs no partial
// index. Attribution only ever takes effect going forward: consumption from
// before the interval belongs to whoever held it then, because that period may
// already be settled and paid out.
type ManagerCustomer struct {
	Id            int64  `json:"id" gorm:"primaryKey"`
	ManagerId     int    `json:"manager_id" gorm:"not null;index:idx_mc_mgr,priority:1"`
	UserId        int    `json:"user_id" gorm:"not null;index;uniqueIndex:uk_mc_open,priority:1"`
	EffectiveFrom int64  `json:"effective_from" gorm:"bigint;not null;index:idx_mc_mgr,priority:2"`
	EffectiveTo   int64  `json:"effective_to" gorm:"bigint;not null;default:0;uniqueIndex:uk_mc_open,priority:2"`
	Source        string `json:"source" gorm:"type:varchar(32);not null;default:''"`
	CreatedBy     int    `json:"created_by" gorm:"not null;default:0"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint;index"`
}

func (ManagerCustomer) TableName() string { return "cp_manager_customer" }

// UserTopupRatio records what a customer actually paid per unit of quota, so a
// discounted top-up (0.8 quota per dollar) or a premium one (1.2) is reflected
// in recognized revenue. It multiplies revenue only, never cost: upstream
// purchase cost does not change because a customer bought quota on sale.
//
// Kept in this package's own table rather than dto.UserSetting, which is an
// upstream file and would conflict on merge.
type UserTopupRatio struct {
	Id            int64  `json:"id" gorm:"primaryKey"`
	UserId        int    `json:"user_id" gorm:"not null;index;uniqueIndex:uk_utr_open,priority:1"`
	RatioMill     int64  `json:"ratio_mill" gorm:"bigint;not null;default:1000"`
	EffectiveFrom int64  `json:"effective_from" gorm:"bigint;not null"`
	EffectiveTo   int64  `json:"effective_to" gorm:"bigint;not null;default:0;uniqueIndex:uk_utr_open,priority:2"`
	Note          string `json:"note" gorm:"type:varchar(255);not null;default:''"`
	CreatedBy     int    `json:"created_by" gorm:"not null;default:0"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint;index"`
}

func (UserTopupRatio) TableName() string { return "cp_user_topup_ratio" }

// Payout records one manual settlement against a manager's accrued share.
// Amount is signed: a negative row reverses an earlier mistake rather than
// deleting it, keeping the trail intact.
type Payout struct {
	Id         int64  `json:"id" gorm:"primaryKey"`
	ManagerId  int    `json:"manager_id" gorm:"not null;index:idx_po_mgr,priority:1"`
	Amount     int64  `json:"amount" gorm:"bigint;not null"`
	Note       string `json:"note" gorm:"type:varchar(255);not null;default:''"`
	OperatorId int    `json:"operator_id" gorm:"not null"`
	CreatedAt  int64  `json:"created_at" gorm:"bigint;index:idx_po_mgr,priority:2"`
}

func (Payout) TableName() string { return "cp_payout" }

// Claim is a manager's request to take over a customer who did not arrive
// through that manager's invite code. PendingKey is non-nil only while the
// claim is pending, so the unique index blocks two managers from having an open
// claim on the same customer while still allowing many resolved ones. This
// nullable-unique shape mirrors model.SystemTask.ActiveKey.
type Claim struct {
	Id         int64   `json:"id" gorm:"primaryKey"`
	ManagerId  int     `json:"manager_id" gorm:"not null;index"`
	UserId     int     `json:"user_id" gorm:"not null;index"`
	Username   string  `json:"username" gorm:"type:varchar(64);not null;default:''"`
	Status     string  `json:"status" gorm:"type:varchar(16);not null;index"`
	PendingKey *string `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	Reason     string  `json:"reason" gorm:"type:varchar(255);not null;default:''"`
	ReviewNote string  `json:"review_note" gorm:"type:varchar(255);not null;default:''"`
	ReviewedBy int     `json:"reviewed_by" gorm:"not null;default:0"`
	ReviewedAt int64   `json:"reviewed_at" gorm:"bigint;not null;default:0"`
	CreatedAt  int64   `json:"created_at" gorm:"bigint;index"`
}

func (Claim) TableName() string { return "cp_claim" }

// MonthlyRollup is the last-resort ledger. It lives in the main database even
// when the per-bucket ledger lives beside the logs, so losing or rebuilding the
// log database still leaves settled monthly totals intact. SealedAt != 0 marks
// a month closed, which makes the recompute action refuse to touch it.
type MonthlyRollup struct {
	Id          int64 `json:"id" gorm:"primaryKey"`
	YearMonth   int   `json:"year_month" gorm:"not null;uniqueIndex:uk_mr,priority:1"`
	ManagerId   int   `json:"manager_id" gorm:"not null;uniqueIndex:uk_mr,priority:2"`
	ProfitQuota int64 `json:"profit_quota" gorm:"bigint;not null;default:0"`
	ShareQuota  int64 `json:"share_quota" gorm:"bigint;not null;default:0"`
	SealedAt    int64 `json:"sealed_at" gorm:"bigint;not null;default:0"`
}

func (MonthlyRollup) TableName() string { return "cp_monthly_rollup" }

// GroupCostDaily powers the group average cost chart. It is a daily rollup so
// the chart does not have to scan millions of ledger buckets, and it lives in
// the main database so the trend line survives log cleanup.
//
// ChannelId 0 is the all-channel total for that group and day; the per-channel
// rows sit alongside it for the multi-select comparison.
type GroupCostDaily struct {
	Id         int64  `json:"id" gorm:"primaryKey"`
	StatDate   int    `json:"stat_date" gorm:"not null;uniqueIndex:uk_gcd,priority:1"`
	GroupName  string `json:"group_name" gorm:"type:varchar(64);not null;default:'';uniqueIndex:uk_gcd,priority:2"`
	ChannelId  int    `json:"channel_id" gorm:"not null;default:0;uniqueIndex:uk_gcd,priority:3"`
	CostQuota  int64  `json:"cost_quota" gorm:"bigint;not null;default:0"`
	GrossQuota int64  `json:"gross_quota" gorm:"bigint;not null;default:0"`
	BaseQuota  int64  `json:"base_quota" gorm:"bigint;not null;default:0"`
	LogCount   int64  `json:"log_count" gorm:"bigint;not null;default:0"`
	UpdatedAt  int64  `json:"updated_at" gorm:"bigint"`
}

func (GroupCostDaily) TableName() string { return "cp_group_cost_daily" }

// ---------------------------------------------------------------------------
// Ledger database: profit flow and settlement state
// ---------------------------------------------------------------------------

// ProfitLedger is one aggregated bucket of settled profit. Buckets are hourly
// and keyed finely enough that root can drill from a manager down to a single
// customer's spend on one model of one channel.
//
// CostId and ConfigId pin which cost version and which global config produced
// the numbers, so any row can be fully explained after the fact.
//
// ManagerId 0 means the customer had no attribution at settlement time. Those
// rows are still written, because the group cost dashboard needs site-wide
// volume; manager-facing queries filter them out.
//
// All money columns are int64: Log.Quota is a 32-bit int (Int32 in the
// ClickHouse DDL) and summing millions of rows would overflow it.
type ProfitLedger struct {
	Id          int64  `json:"id" gorm:"primaryKey"`
	PeriodStart int64  `json:"period_start" gorm:"bigint;not null;index:idx_pl_period;uniqueIndex:uk_pl_bucket,priority:1"`
	ManagerId   int    `json:"manager_id" gorm:"not null;index:idx_pl_mgr,priority:1;uniqueIndex:uk_pl_bucket,priority:2"`
	UserId      int    `json:"user_id" gorm:"not null;uniqueIndex:uk_pl_bucket,priority:3"`
	ChannelId   int    `json:"channel_id" gorm:"not null;default:0;uniqueIndex:uk_pl_bucket,priority:4"`
	ModelName   string `json:"model_name" gorm:"type:varchar(128);not null;default:'';uniqueIndex:uk_pl_bucket,priority:5"`
	GroupName   string `json:"group_name" gorm:"type:varchar(64);not null;default:'';uniqueIndex:uk_pl_bucket,priority:6"`
	CostId      int64  `json:"cost_id" gorm:"bigint;not null;default:0;uniqueIndex:uk_pl_bucket,priority:7"`
	ConfigId    int64  `json:"config_id" gorm:"bigint;not null;default:0;uniqueIndex:uk_pl_bucket,priority:8"`

	GrossQuota   int64 `json:"gross_quota" gorm:"bigint;not null;default:0"`
	RevenueQuota int64 `json:"revenue_quota" gorm:"bigint;not null;default:0"`
	BaseQuota    int64 `json:"base_quota" gorm:"bigint;not null;default:0"`
	CostQuota    int64 `json:"cost_quota" gorm:"bigint;not null;default:0"`
	ProfitQuota  int64 `json:"profit_quota" gorm:"bigint;not null;default:0"`
	ShareQuota   int64 `json:"share_quota" gorm:"bigint;not null;default:0"`

	LogCount        int64 `json:"log_count" gorm:"bigint;not null;default:0"`
	RefundCount     int64 `json:"refund_count" gorm:"bigint;not null;default:0"`
	NoCostCount     int64 `json:"no_cost_count" gorm:"bigint;not null;default:0"`
	NoCostQuota     int64 `json:"no_cost_quota" gorm:"bigint;not null;default:0"`
	DerivedCount    int64 `json:"derived_count" gorm:"bigint;not null;default:0"`
	RatioFallback   int64 `json:"ratio_fallback" gorm:"bigint;not null;default:0"`
	UnpriceableCnt  int64 `json:"unpriceable_count" gorm:"bigint;not null;default:0"`
	UnpriceableQuot int64 `json:"unpriceable_quota" gorm:"bigint;not null;default:0"`

	CreatedAt int64 `json:"created_at" gorm:"bigint;index:idx_pl_mgr,priority:2"`
}

func (ProfitLedger) TableName() string { return "cp_profit_ledger" }

// SettlementCursor is a single row (Id == 1) holding the watermark: every log
// with created_at < SettledBefore has been settled and must never be recomputed
// implicitly.
type SettlementCursor struct {
	Id            int   `json:"id" gorm:"primaryKey"`
	SettledBefore int64 `json:"settled_before" gorm:"bigint;not null;default:0"`
	UpdatedAt     int64 `json:"updated_at" gorm:"bigint"`
}

func (SettlementCursor) TableName() string { return "cp_settlement_cursor" }

// CursorRowId is the primary key of the singleton SettlementCursor row.
const CursorRowId = 1

// SettlementRun is the audit trail for one settlement window. EarliestLogAt
// records the oldest surviving log at the time of the run: if it ever exceeds
// the watermark, logs were cleaned up before being settled and that profit is
// unrecoverable, so the panel must surface it loudly.
type SettlementRun struct {
	Id            int64  `json:"id" gorm:"primaryKey"`
	WindowFrom    int64  `json:"window_from" gorm:"bigint;not null;uniqueIndex:uk_sr_window,priority:1"`
	WindowTo      int64  `json:"window_to" gorm:"bigint;not null;uniqueIndex:uk_sr_window,priority:2"`
	Status        string `json:"status" gorm:"type:varchar(16);not null;index"`
	ScannedLogs   int64  `json:"scanned_logs" gorm:"bigint;not null;default:0"`
	BucketCount   int64  `json:"bucket_count" gorm:"bigint;not null;default:0"`
	NoCostQuota   int64  `json:"no_cost_quota" gorm:"bigint;not null;default:0"`
	EarliestLogAt int64  `json:"earliest_log_at" gorm:"bigint;not null;default:0"`
	Error         string `json:"error" gorm:"type:text"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint;index"`
}

func (SettlementRun) TableName() string { return "cp_settlement_run" }

// mainTables are migrated against model.DB.
func mainTables() []any {
	return []any{
		&ChannelCost{},
		&ChannelSnapshot{},
		&GlobalConfig{},
		&ManagerCustomer{},
		&UserTopupRatio{},
		&Payout{},
		&Claim{},
		&MonthlyRollup{},
		&GroupCostDaily{},
	}
}

// ledgerTables are migrated against the ledger handle, which is the log
// database by default. They must stay in one database together: settlement
// rewrites the ledger, advances the cursor, and records the run in a single
// transaction.
func ledgerTables() []any {
	return []any{
		&ProfitLedger{},
		&SettlementCursor{},
		&SettlementRun{},
	}
}
