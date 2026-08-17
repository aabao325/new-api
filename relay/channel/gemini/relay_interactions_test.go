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
