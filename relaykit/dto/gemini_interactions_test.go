package dto

import (
	"testing"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGeminiInteractionsRequestPreservesMultimodalPayloadWhenMapped(t *testing.T) {
	var request GeminiInteractionsRequest
	err := kitutil.Unmarshal([]byte(`{
		"model":"gemini-omni-flash-preview",
		"stream":false,
		"input":[{"role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_video","video_url":"https://example.test/video.mp4"}]}],
		"generation_config":{"response_modalities":["TEXT","IMAGE"]}
	}`), &request)
	require.NoError(t, err)
	require.NotNil(t, request.Stream)
	assert.False(t, *request.Stream)
	assert.Contains(t, request.GetTokenCountMeta().CombineText, "describe this")

	request.SetModelName("gemini-omni-flash-preview-001")
	encoded, err := kitutil.Marshal(&request)
	require.NoError(t, err)
	assert.Equal(t, "gemini-omni-flash-preview-001", gjson.GetBytes(encoded, "model").String())
	assert.Equal(t, "input_video", gjson.GetBytes(encoded, "input.0.content.1.type").String())
	assert.Equal(t, "https://example.test/video.mp4", gjson.GetBytes(encoded, "input.0.content.1.video_url").String())
	assert.Equal(t, "IMAGE", gjson.GetBytes(encoded, "generation_config.response_modalities.1").String())
}
