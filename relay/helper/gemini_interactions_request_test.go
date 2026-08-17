package helper

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAndValidateGeminiInteractionsRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("preserves multimodal request", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/interactions", http.NoBody)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"gemini-omni-flash-preview","input":[{"type":"video","uri":"gs://bucket/video.mp4"}]}`))

		request, err := GetAndValidateGeminiInteractionsRequest(c)
		require.NoError(t, err)
		assert.Equal(t, "gemini-omni-flash-preview", request.Model)
		assert.Contains(t, request.Body, "input")
	})

	t.Run("rejects nested max output token overflow", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/interactions", http.NoBody)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Body = io.NopCloser(strings.NewReader(`{"model":"gemini-omni-flash-preview","generation_config":{"max_output_tokens":` + fmt.Sprint(math.MaxInt32) + `}}`))

		request, err := GetAndValidateGeminiInteractionsRequest(c)
		assert.Nil(t, request)
		require.EqualError(t, err, "max_output_tokens is invalid")
	})
}
