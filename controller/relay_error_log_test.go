package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessChannelErrorUsesSnapshotWithoutLeakingChannelMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousErrorLogEnabled := constant.ErrorLogEnabled

	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Log{}))
	model.DB, model.LOG_DB = database, database
	common.RedisEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	constant.ErrorLogEnabled = true
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		constant.ErrorLogEnabled = previousErrorLogEnabled
		require.NoError(t, sqlDB.Close())
	})

	require.NoError(t, database.Create(&model.User{Id: 7, Username: "log-owner", Group: "default"}).Error)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 7)
	ctx.Set("username", "log-owner")
	ctx.Set("token_name", "test-token")
	ctx.Set("token_id", 11)
	ctx.Set("original_model", "gpt-test")
	ctx.Set("group", "default")
	ctx.Set("channel_id", 202)
	ctx.Set("channel_name", "mutable-context-channel")
	ctx.Set("channel_type", 9)
	ctx.Set("use_channel", []string{"101"})
	common.SetContextKey(ctx, constant.ContextKeyRequestStartTime, time.Now().Add(-time.Second))

	channelSnapshot := types.ChannelError{
		ChannelId:   101,
		ChannelType: 1,
		ChannelName: "snapshot-channel",
		AutoBan:     false,
	}
	apiErr := types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)

	processChannelError(ctx, channelSnapshot, apiErr, nil)

	var stored model.Log
	require.NoError(t, database.First(&stored).Error)
	assert.Equal(t, channelSnapshot.ChannelId, stored.ChannelId)
	storedOther, err := common.StrToMap(stored.Other)
	require.NoError(t, err)
	assert.Equal(t, float64(http.StatusBadGateway), storedOther["status_code"])
	for _, key := range []string{"channel_id", "channel_name", "channel_type"} {
		assert.NotContains(t, storedOther, key)
	}
	adminInfo, ok := storedOther["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"101"}, adminInfo["use_channel"])

	// 对照日志：同一用户、同一令牌下的消费日志。用来证明过滤只排除错误日志，
	// 而不是把整个查询结果清空。
	require.NoError(t, database.Create(&model.Log{
		UserId:    7,
		Username:  "log-owner",
		CreatedAt: common.GetTimestamp(),
		Type:      model.LogTypeConsume,
		TokenName: "test-token",
		TokenId:   11,
		ModelName: "gpt-test",
		Group:     "default",
	}).Error)

	// 用户按"错误"类型查 → 查不到。
	errorTypeLogs, errorTypeTotal, err := model.GetUserLogs(7, model.LogTypeError, 0, 0, "", "", 0, 10, "", "", "")
	require.NoError(t, err)
	assert.Zero(t, errorTypeTotal)
	assert.Empty(t, errorTypeLogs)

	// 用户选"全部类型"（LogTypeUnknown，此时后端不附加类型条件）→ 结果里也不能有错误日志。
	// 这是本次改动的核心回归点：过滤必须落在查询上，只在 controller 拦 type=5 挡不住这条路径。
	allTypeLogs, allTypeTotal, err := model.GetUserLogs(7, model.LogTypeUnknown, 0, 0, "", "", 0, 10, "", "", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), allTypeTotal)
	require.Len(t, allTypeLogs, 1)
	assert.Equal(t, model.LogTypeConsume, allTypeLogs[0].Type)

	// 令牌鉴权的自查接口（/api/log/token）是另一个入口，同样不能返回错误日志。
	tokenLogs, err := model.GetLogByTokenId(11)
	require.NoError(t, err)
	require.Len(t, tokenLogs, 1)
	assert.Equal(t, model.LogTypeConsume, tokenLogs[0].Type)

	// 管理员侧不受影响：错误日志仍在库里，admin_info 完整。
	// 这里直接查库而不走 GetAllLogs，因为后者会查 channels 表，本测试未迁移该表。
	var adminVisible []model.Log
	require.NoError(t, database.Where("type = ?", model.LogTypeError).Find(&adminVisible).Error)
	require.Len(t, adminVisible, 1)
	assert.Equal(t, channelSnapshot.ChannelId, adminVisible[0].ChannelId)
	adminVisibleOther, err := common.StrToMap(adminVisible[0].Other)
	require.NoError(t, err)
	assert.Contains(t, adminVisibleOther, "admin_info")
}
