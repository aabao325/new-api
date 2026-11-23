package dto

import (
	"encoding/json"
	"testing"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Gemini accepts a part whose text is the empty string but rejects a part that
// has no data oneof at all. A tool-call history echoed back by the client can
// therefore contain {"text":""}, and it must survive the parse/re-marshal round
// trip instead of collapsing to {} (which upstream answers with
// "required oneof field 'data' must have one initialized field").
func TestGeminiPartKeepsExplicitEmptyText(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
		absent   []string
	}{
		{
			name:     "empty text only",
			input:    `{"text":""}`,
			expected: []string{`"text":""`},
		},
		{
			name:     "empty text with thought signature",
			input:    `{"text":"","thoughtSignature":"c2ln"}`,
			expected: []string{`"text":""`, `"thoughtSignature":"c2ln"`},
		},
		{
			name:     "empty text keeps its thought flag",
			input:    `{"text":"","thought":true}`,
			expected: []string{`"text":""`, `"thought":true`},
		},
		{
			name:     "function response carries no text",
			input:    `{"functionResponse":{"name":"lookup_probe","response":{"result":"CARBON-OK"}}}`,
			expected: []string{`"functionResponse":{`},
			absent:   []string{`"text"`},
		},
		{
			name:     "non empty text",
			input:    `{"text":"CARBON-OK"}`,
			expected: []string{`"text":"CARBON-OK"`},
		},
		{
			name:     "function call carries no text",
			input:    `{"functionCall":{"name":"lookup_probe","args":{"id":"carbon-check-1"}}}`,
			expected: []string{`"functionCall":{`},
			absent:   []string{`"text"`},
		},
		{
			name:     "inline data carries no text",
			input:    `{"inlineData":{"mimeType":"image/png","data":"aGk="}}`,
			expected: []string{`"inlineData":{`},
			absent:   []string{`"text"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var part GeminiPart
			require.NoError(t, kitutil.Unmarshal([]byte(tc.input), &part))

			data, err := kitutil.Marshal(part)
			require.NoError(t, err)

			for _, want := range tc.expected {
				assert.Contains(t, string(data), want)
			}
			for _, unwanted := range tc.absent {
				assert.NotContains(t, string(data), unwanted)
			}
		})
	}
}

func TestGeminiChatRequestKeepsExplicitEmptyTextPart(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[{"text":"CARBON-OK"},{"text":""}]}]}`

	var request GeminiChatRequest
	require.NoError(t, kitutil.Unmarshal([]byte(body), &request))
	require.Len(t, request.Contents, 1)
	require.Len(t, request.Contents[0].Parts, 2)

	data, err := kitutil.Marshal(request)
	require.NoError(t, err)

	var roundTrip struct {
		Contents []struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	require.NoError(t, kitutil.Unmarshal(data, &roundTrip))
	require.Len(t, roundTrip.Contents, 1)
	require.Len(t, roundTrip.Contents[0].Parts, 2)
	assert.JSONEq(t, `{"text":"CARBON-OK"}`, string(roundTrip.Contents[0].Parts[0]))
	assert.JSONEq(t, `{"text":""}`, string(roundTrip.Contents[0].Parts[1]))
}
