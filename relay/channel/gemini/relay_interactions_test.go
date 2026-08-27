package gemini

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiInteractionsRequestURL(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeGeminiInteractions,
		OriginModelName: "gemini-omni-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    "https://generativelanguage.googleapis.com",
			UpstreamModelName: "gemini-omni-flash-preview",
		},
	}

	url, err := (&Adaptor{}).GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://generativelanguage.googleapis.com/v1beta/interactions", url)

	info.IsStream = true
	url, err = (&Adaptor{}).GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://generativelanguage.googleapis.com/v1beta/interactions?alt=sse", url)
	assert.True(t, info.DisablePing)
}

func TestGeminiInteractionsHandlerPassesThroughResponseAndReadsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/interactions", nil)

	payload := []byte(`{"id":"interaction-1","outputs":[{"type":"text","text":"hello"}],"usage":{"total_input_tokens":12,"total_output_tokens":8,"total_tokens":20}}`)
	usage, newAPIError := GeminiInteractionsHandler(c, newGeminiInteractionsRelayInfo(false), &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	})

	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	assert.Equal(t, 12, usage.PromptTokens)
	assert.Equal(t, 8, usage.CompletionTokens)
	assert.Equal(t, 20, usage.TotalTokens)
	require.NotNil(t, usage.BillingUsage)
	assert.False(t, usage.BillingUsage.Estimated)
	assert.JSONEq(t, string(payload), recorder.Body.String())
}

func TestGeminiInteractionsStreamHandlerPassesThroughSSEAndReadsFinalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/interactions", nil)

	stream := "event: interaction.delta\n" +
		"data: {\"delta\":\"hello\"}\n\n" +
		"event: interaction.completed\n" +
		"data: {\"interaction\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
	usage, newAPIError := GeminiInteractionsStreamHandler(c, newGeminiInteractionsRelayInfo(true), &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(stream)),
	})

	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.TotalTokens)
	assert.Equal(t, stream, recorder.Body.String())
}

func newGeminiInteractionsRelayInfo(stream bool) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		IsStream:        stream,
		RelayMode:       relayconstant.RelayModeGeminiInteractions,
		RelayFormat:     types.RelayFormatGemini,
		RequestURLPath:  "/v1beta/interactions",
		OriginModelName: "gemini-omni-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-omni-flash-preview",
		},
	}
}

// TestGeminiInteractionsUsageByModality locks the wire format the Interactions
// API actually returns: per-modality token counts arrive as arrays with lower
// case labels, and thought/cache totals use total_* names that the flat
// generateContent aliases do not cover. Billing prices video output separately,
// so losing these fields silently undercharges every video request.
func TestGeminiInteractionsUsageByModality(t *testing.T) {
	payload := []byte(`{
		"usage": {
			"total_tokens": 117337,
			"total_input_tokens": 57703,
			"input_tokens_by_modality": [
				{"modality": "video", "tokens": 57600},
				{"modality": "text", "tokens": 103}
			],
			"total_cached_tokens": 0,
			"total_output_tokens": 59109,
			"output_tokens_by_modality": [
				{"modality": "video", "tokens": 57920}
			],
			"total_tool_use_tokens": 0,
			"total_thought_tokens": 525,
			"raw_prompt_token": 34330
		}
	}`)

	usage, found := geminiInteractionsUsageFromPayload(payload, &relaycommon.RelayInfo{})
	require.True(t, found)
	require.NotNil(t, usage)

	assert.Equal(t, 57703, usage.PromptTokens)
	assert.Equal(t, 59109, usage.CompletionTokens)
	assert.Equal(t, 117337, usage.TotalTokens)

	assert.Equal(t, 57600, usage.PromptTokensDetails.VideoTokens)
	assert.Equal(t, 103, usage.PromptTokensDetails.TextTokens)
	assert.Equal(t, 57920, usage.CompletionTokenDetails.VideoTokens)
	assert.Equal(t, 525, usage.CompletionTokenDetails.ReasoningTokens)

	require.NotNil(t, usage.BillingUsage)
	require.NotNil(t, usage.BillingUsage.GeminiUsageMetadata)
	metadata := usage.BillingUsage.GeminiUsageMetadata
	assert.Equal(t, 525, metadata.ThoughtsTokenCount)
	assert.Equal(t, 59109-525, metadata.CandidatesTokenCount)

	var promptVideo, candidateVideo int
	for _, detail := range metadata.PromptTokensDetails {
		if detail.Modality == "VIDEO" {
			promptVideo = detail.TokenCount
		}
	}
	for _, detail := range metadata.CandidatesTokensDetails {
		if detail.Modality == "VIDEO" {
			candidateVideo = detail.TokenCount
		}
	}
	assert.Equal(t, 57600, promptVideo)
	assert.Equal(t, 57920, candidateVideo)
}

// TestGeminiInteractionsUsageScalarAliasesWin guards the overlay order: when an
// upstream reports flat scalar counts, the modality array must not clobber them.
func TestGeminiInteractionsUsageScalarAliasesWin(t *testing.T) {
	payload := []byte(`{
		"usage": {
			"input_tokens": 300,
			"output_tokens": 200,
			"input_audio_tokens": 111,
			"output_video_tokens": 222,
			"input_tokens_by_modality": [{"modality": "audio", "tokens": 999}],
			"output_tokens_by_modality": [{"modality": "video", "tokens": 888}]
		}
	}`)

	usage, found := geminiInteractionsUsageFromPayload(payload, &relaycommon.RelayInfo{})
	require.True(t, found)
	assert.Equal(t, 111, usage.PromptTokensDetails.AudioTokens)
	assert.Equal(t, 222, usage.CompletionTokenDetails.VideoTokens)
}
