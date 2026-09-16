package costprofit

import (
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the panel's tripwire on upstream.
//
// Settlement reads upstream's logs, channels and users but owns none of them,
// and it reconstructs per-request usage arithmetically from fields upstream
// writes for its own reasons. None of that is a published API, so upstream is
// free to rename a column or renumber a constant in a release that merges
// cleanly and then quietly produces wrong money.
//
// Every assertion below is a dependency that would fail silently. Run
// `go test ./costprofit/...` after each upstream merge: a failure here means
// read the diff before trusting a single figure in the panel.

// TestUpstreamLogContract pins the log fields settlement selects.
func TestUpstreamLogContract(t *testing.T) {
	logType := reflect.TypeFor[model.Log]()

	// Settlement selects exactly these columns. Anything absent from this list
	// is deliberately not read: content and ip are large and unused, and
	// request_id cannot be a cursor because one HTTP request reuses it across
	// several billing rows.
	for _, tc := range []struct {
		field string
		kind  reflect.Kind
		why   string
	}{
		{"UserId", reflect.Int, "attribution and the per-customer top-up ratio"},
		{"CreatedAt", reflect.Int64, "window bounds and which config version applied"},
		{"Type", reflect.Int, "consumption versus refund decides the sign"},
		{"Quota", reflect.Int, "the charged amount every figure derives from"},
		{"ChannelId", reflect.Int, "which channel's cost applies"},
		{"ModelName", reflect.String, "per-model cost overrides"},
		{"Group", reflect.String, "group ratio fallback and the cost dashboard"},
		{"Other", reflect.String, "carries group_ratio and model_price as JSON"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			field, ok := logType.FieldByName(tc.field)
			require.True(t, ok, "model.Log lost %s, needed for %s", tc.field, tc.why)
			assert.Equal(t, tc.kind, field.Type.Kind(), "model.Log.%s changed type, needed for %s", tc.field, tc.why)
		})
	}

	// Quota being a 32-bit int is why every ledger column is int64: summing
	// millions of rows into the log's own width would overflow.
	quota, _ := logType.FieldByName("Quota")
	require.Equal(t, reflect.Int, quota.Type.Kind(),
		"model.Log.Quota widened; revisit the int64 accumulation in settlement")

	// ChannelName has no column of its own (gorm `->` makes it read-only and
	// AutoMigrate never creates it); upstream populates it at query time. The
	// panel therefore takes channel names from cp_channel_snapshot, which also
	// survives a rename or deletion. If this tag ever changes, selecting the
	// column directly may become possible and the snapshot join is redundant.
	channelName, ok := logType.FieldByName("ChannelName")
	require.True(t, ok, "model.Log.ChannelName disappeared")
	assert.Equal(t, "->", channelName.Tag.Get("gorm"),
		"model.Log.ChannelName is no longer a read-only virtual field")
}

// TestUpstreamLogTypeContract pins the numeric log types settlement filters on.
func TestUpstreamLogTypeContract(t *testing.T) {
	// settledLogTypes() hardcodes these so the panel keeps its own vocabulary;
	// this is the assertion that keeps the two in step. The upstream constants
	// are explicitly not iota-based ("don't use iota, avoid change log type
	// value"), so a change here is a deliberate upstream renumbering.
	assert.Equal(t, 2, model.LogTypeConsume, "consumption log type changed")
	assert.Equal(t, 6, model.LogTypeRefund, "refund log type changed")
	assert.ElementsMatch(t, []int{model.LogTypeConsume, model.LogTypeRefund}, settledLogTypes(),
		"settledLogTypes() drifted from the upstream log type constants")
}

// TestUpstreamLogOtherContract pins where the usage-reversal inputs live.
func TestUpstreamLogOtherContract(t *testing.T) {
	// group_ratio and model_price must stay at the top level of the JSON.
	// Upstream writes them through SetPublic, which is the tier the log owner is
	// allowed to see; had they been written as admin- or root-scoped, they would
	// be nested under an admin_info/root_info object and reading them from the
	// top level would silently yield zero, marking every row unpriceable.
	other := model.NewLogOther()
	require.True(t, other.SetPublic("group_ratio", 0.9), "group_ratio is no longer a writable public key")
	require.True(t, other.SetPublic("model_price", 0.1), "model_price is no longer a writable public key")

	// Round-trip through the stored representation rather than inspecting the
	// struct: settlement only ever sees this string, read back out of the logs
	// table.
	var decoded map[string]any
	require.NoError(t, common.UnmarshalJsonStr(other.JSONString(), &decoded))
	assert.Equal(t, 0.9, decoded["group_ratio"], "group_ratio is no longer readable at the top level")
	assert.Equal(t, 0.1, decoded["model_price"], "model_price is no longer readable at the top level")
}

// TestUpstreamQuotaContract pins the constant the usage reversal divides by.
func TestUpstreamQuotaContract(t *testing.T) {
	// Per-unit billing is quota = model_price * QuotaPerUnit * group_ratio *
	// other ratios, which the panel inverts to recover a request count or a
	// duration in seconds. A change to this scale silently rescales every
	// per-request and per-second cost.
	assert.Equal(t, 500000.0, common.QuotaPerUnit, "QuotaPerUnit changed; per-unit cost reversal is rescaled")

	// Aggregated buckets are converted with WalletQuotaFromDecimalStrict, whose
	// domain must stay wider than int32: the int32-bounded helpers saturate at
	// MaxQuota, which would turn a large but legitimate monthly total into a
	// capped number instead of an error.
	assert.Greater(t, int64(common.MaxWalletQuota), int64(common.MaxQuota),
		"the wallet quota domain no longer exceeds int32; aggregate conversion would saturate")
}

// TestUpstreamChannelContract pins the channel fields the snapshot copies.
func TestUpstreamChannelContract(t *testing.T) {
	channelType := reflect.TypeFor[model.Channel]()
	for _, tc := range []struct {
		field string
		kind  reflect.Kind
	}{
		{"Id", reflect.Int},
		{"Name", reflect.String},
		{"Status", reflect.Int},
		{"Type", reflect.Int},
	} {
		field, ok := channelType.FieldByName(tc.field)
		require.True(t, ok, "model.Channel lost %s, needed for the channel snapshot", tc.field)
		assert.Equal(t, tc.kind, field.Type.Kind(), "model.Channel.%s changed type", tc.field)
	}

	// The panel renders these three states; 0 is reserved for "deleted" in
	// cp_channel_snapshot, which is why upstream's warning that 0 is never a
	// real status matters here too.
	assert.Equal(t, 1, common.ChannelStatusEnabled)
	assert.Equal(t, 2, common.ChannelStatusManuallyDisabled)
	assert.Equal(t, 3, common.ChannelStatusAutoDisabled)
}
