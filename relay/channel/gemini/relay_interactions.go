package gemini

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func GeminiInteractionsHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	logger.LogDebug(c, "Gemini interactions response body: %s", responseBody)
	usage, found := geminiInteractionsUsageFromPayload(responseBody, info)
	if !found {
		usage = estimatedGeminiInteractionsUsage(info)
	}
	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func GeminiInteractionsStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	for key, values := range resp.Header {
		if !service.ShouldCopyUpstreamHeader(c, key, values) {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)

	var latestUsage *dto.Usage
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := c.Writer.Write(line); writeErr != nil {
				logger.LogDebug(c, "Gemini interactions stream client disconnected: %s", writeErr.Error())
				break
			}
			data := bytes.TrimSpace(line)
			if bytes.HasPrefix(data, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("data:")))
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					if usage, found := geminiInteractionsUsageFromPayload(payload, info); found {
						latestUsage = usage
					}
				}
			} else if len(data) > 0 && data[0] == '{' {
				if usage, found := geminiInteractionsUsageFromPayload(data, info); found {
					latestUsage = usage
				}
			}
			c.Writer.Flush()
		}
		if err == nil {
			continue
		}
		if err != io.EOF {
			logger.LogError(c, fmt.Sprintf("read Gemini interactions stream failed: %s", err.Error()))
		}
		break
	}

	if latestUsage != nil {
		return latestUsage, nil
	}
	return estimatedGeminiInteractionsUsage(info), nil
}

func geminiInteractionsUsageFromPayload(payload []byte, info *relaycommon.RelayInfo) (*dto.Usage, bool) {
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return nil, false
	}

	var usageRoot gjson.Result
	for _, path := range []string{
		"usage", "usage_metadata", "usageMetadata",
		"interaction.usage", "interaction.usage_metadata", "interaction.usageMetadata",
		"response.usage", "result.usage",
	} {
		candidate := root.Get(path)
		if candidate.Exists() && candidate.Type != gjson.Null {
			usageRoot = candidate
			break
		}
	}
	if !usageRoot.Exists() || usageRoot.Type == gjson.Null {
		return nil, false
	}

	usage := &dto.Usage{
		PromptTokens:     geminiInteractionToken(usageRoot, info, "input_tokens", "total_input_tokens", "inputTokenCount", "input_token_count", "prompt_tokens", "promptTokenCount", "prompt_token_count"),
		CompletionTokens: geminiInteractionToken(usageRoot, info, "output_tokens", "total_output_tokens", "outputTokenCount", "output_token_count", "candidatesTokenCount", "candidates_token_count"),
		TotalTokens:      geminiInteractionToken(usageRoot, info, "total_tokens", "totalTokenCount", "total_token_count"),
	}
	usage.PromptTokensDetails.CachedTokens = geminiInteractionToken(usageRoot, info, "cached_tokens", "input_tokens_details.cached_tokens", "cachedContentTokenCount", "cached_content_token_count")
	usage.PromptTokensDetails.ImageTokens = geminiInteractionToken(usageRoot, info, "input_image_tokens", "input_tokens_details.image_tokens", "inputImageTokenCount")
	usage.PromptTokensDetails.AudioTokens = geminiInteractionToken(usageRoot, info, "input_audio_tokens", "input_tokens_details.audio_tokens", "inputAudioTokenCount")
	usage.CompletionTokenDetails.ReasoningTokens = geminiInteractionToken(usageRoot, info, "reasoning_tokens", "output_tokens_details.reasoning_tokens", "thoughtsTokenCount", "thoughts_token_count")
	usage.CompletionTokenDetails.ImageTokens = geminiInteractionToken(usageRoot, info, "output_image_tokens", "output_tokens_details.image_tokens", "outputImageTokenCount")
	usage.CompletionTokenDetails.AudioTokens = geminiInteractionToken(usageRoot, info, "output_audio_tokens", "output_tokens_details.audio_tokens", "outputAudioTokenCount")
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0 {
		return nil, false
	}
	metadata := &dto.GeminiUsageMetadata{
		PromptTokenCount:        usage.PromptTokens,
		CandidatesTokenCount:    usage.CompletionTokens - usage.CompletionTokenDetails.ReasoningTokens,
		TotalTokenCount:         usage.TotalTokens,
		ThoughtsTokenCount:      usage.CompletionTokenDetails.ReasoningTokens,
		CachedContentTokenCount: usage.PromptTokensDetails.CachedTokens,
	}
	if metadata.CandidatesTokenCount < 0 {
		metadata.CandidatesTokenCount = 0
	}
	for modality, count := range map[string]int{
		"IMAGE": usage.PromptTokensDetails.ImageTokens,
		"AUDIO": usage.PromptTokensDetails.AudioTokens,
	} {
		if count > 0 {
			metadata.PromptTokensDetails = append(metadata.PromptTokensDetails, dto.GeminiPromptTokensDetails{Modality: modality, TokenCount: count})
		}
	}
	for modality, count := range map[string]int{
		"IMAGE": usage.CompletionTokenDetails.ImageTokens,
		"AUDIO": usage.CompletionTokenDetails.AudioTokens,
	} {
		if count > 0 {
			metadata.CandidatesTokensDetails = append(metadata.CandidatesTokensDetails, dto.GeminiPromptTokensDetails{Modality: modality, TokenCount: count})
		}
	}
	usage.BillingUsage = dto.NewGeminiChatBillingUsage(metadata)
	return usage, true
}

func geminiInteractionToken(usageRoot gjson.Result, info *relaycommon.RelayInfo, paths ...string) int {
	for _, path := range paths {
		value := usageRoot.Get(path)
		if !value.Exists() || value.Type != gjson.Number {
			continue
		}
		amount := value.Float()
		if amount < 0 {
			common.SysError(fmt.Sprintf("Gemini interactions returned negative token count for %s: %g", path, amount))
			return 0
		}
		tokens, clamp := common.QuotaFromFloatChecked(amount)
		if clamp != nil && info.QuotaClamp == nil {
			info.QuotaClamp = clamp
		}
		return tokens
	}
	return 0
}

func estimatedGeminiInteractionsUsage(info *relaycommon.RelayInfo) *dto.Usage {
	usage := &dto.Usage{PromptTokens: info.GetEstimatePromptTokens()}
	usage.TotalTokens = usage.PromptTokens
	usage.PromptTokensDetails.TextTokens = usage.PromptTokens
	usage.BillingUsage = dto.NewEstimatedGeminiChatBillingUsage(usage)
	return usage
}
