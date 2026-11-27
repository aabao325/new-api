package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(responsesResponse.Model)
	responseBody = rewriteSGLangResponsesCreatedAt(info, responseBody, "created_at", responsesResponse.CreatedAt)

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := &dto.Usage{}
	service.ApplyResponsesUsage(usage, responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}
	info.ApplyVendorToolUsage(responseBody)

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	for i := range responsesResponse.Output {
		imageCounter.Observe(&responsesResponse.Output[i], &i)
	}
	imageCounter.Commit(info)

	return usage, nil
}

// maxHeldResponsesEvents bounds the lifecycle events held before the first
// output, so a chatty upstream cannot grow the buffer without limit.
const maxHeldResponsesEvents = 256

// responsesStreamPreliminaryEvent reports whether an event only announces the
// response and carries no output yet.
func responsesStreamPreliminaryEvent(eventType string) bool {
	switch eventType {
	case "response.created", "response.in_progress", "response.queued":
		return true
	}
	return false
}

// responsesStreamFailureEvent reports whether an event terminates the response
// as a failure.
func responsesStreamFailureEvent(eventType string) bool {
	switch eventType {
	case "response.failed", "response.error", "error":
		return true
	}
	return false
}

// responsesStreamFailureError converts an upstream failure event into the HTTP
// error the client should receive. It is only used while nothing has been
// written downstream: a rate limit keeps its 429 so callers back off, a request
// error is not retried, and everything else becomes a retryable 503.
func responsesStreamFailureError(event *dto.ResponsesStreamResponse) *types.NewAPIError {
	var upstreamErr *types.OpenAIError
	if event.Response != nil {
		upstreamErr = event.Response.GetOpenAIError()
	}
	if upstreamErr == nil || (upstreamErr.Message == "" && upstreamErr.Code == nil && upstreamErr.Type == "") {
		upstreamErr = dto.GetOpenAIError(event.Error)
	}
	if upstreamErr == nil {
		upstreamErr = &types.OpenAIError{}
	}
	if upstreamErr.Message == "" {
		upstreamErr.Message = fmt.Sprintf("upstream responses stream failed (%s)", event.Type)
	}
	if upstreamErr.Code == nil && upstreamErr.Type != "" {
		// Some failures carry only a type; keep it as the code so the error log
		// and the client see the upstream classification.
		upstreamErr.Code = upstreamErr.Type
	}
	code := ""
	if upstreamErr.Code != nil {
		code = strings.ToLower(fmt.Sprint(upstreamErr.Code))
	}
	errorType := strings.ToLower(upstreamErr.Type)
	switch {
	case code == "rate_limit_exceeded", code == "too_many_requests", code == "insufficient_quota",
		errorType == "too_many_requests", errorType == "rate_limit_error":
		return types.WithOpenAIError(*upstreamErr, http.StatusTooManyRequests)
	case errorType == "invalid_request_error", code == "context_length_exceeded", strings.HasPrefix(code, "invalid_"):
		return types.WithOpenAIError(*upstreamErr, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	default:
		return types.WithOpenAIError(*upstreamErr, http.StatusServiceUnavailable)
	}
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, common.LogText("invalid response or response body"))
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	accumulator := service.NewResponsesUsageAccumulator(info)

	// Events sent before the first output are held back. Until one byte reaches
	// the client the HTTP status is still open, so a stream that fails right
	// after response.created answers with 429/503 and the relay can retry,
	// instead of a 200 that the client can only read as a broken stream. Held
	// events are replayed in order ahead of the first real event.
	type heldEvent struct {
		event dto.ResponsesStreamResponse
		data  string
	}
	var (
		held      []heldEvent
		committed bool
		streamErr *types.NewAPIError
	)

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, common.LogText("failed to unmarshal stream response: %s", err.Error()))
			sr.Error(err)
			return
		}
		if streamResponse.Response != nil {
			data = string(rewriteSGLangResponsesCreatedAt(info, []byte(data), "response.created_at", streamResponse.Response.CreatedAt))
		}
		accumulator.Observe(&streamResponse, common.StringToByteSlice(data))

		if !committed {
			switch {
			case c.Writer.Written():
				// A keepalive ping already committed the HTTP 200.
				committed = true
			case responsesStreamFailureEvent(streamResponse.Type):
				streamErr = responsesStreamFailureError(&streamResponse)
				sr.Stop(streamErr)
				return
			case responsesStreamPreliminaryEvent(streamResponse.Type) && len(held) < maxHeldResponsesEvents:
				held = append(held, heldEvent{event: streamResponse, data: data})
				return
			default:
				committed = true
			}
			for _, buffered := range held {
				if err := helper.ResponseChunkData(c, buffered.event, buffered.data); err != nil {
					sr.Stop(err)
					return
				}
			}
			held = nil
		}

		if err := helper.ResponseChunkData(c, streamResponse, data); err != nil {
			sr.Stop(err)
			return
		}
		if (streamResponse.Type == "response.completed" || streamResponse.Type == "response.done") &&
			streamResponse.Response != nil && info.StreamStatus.ResponseOutcome() == string(relaycommon.ResponseOutcomeCompleted) {
			sr.Done()
		}
	})

	common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, info.StreamStatus)

	// Nothing reached the client, so a stream that ended without output still
	// becomes a retryable HTTP error instead of an empty 200.
	if streamErr == nil && !committed && !c.Writer.Written() && c.Request.Context().Err() == nil {
		streamErr = types.NewOpenAIError(
			fmt.Errorf("upstream responses stream ended before any output"),
			types.ErrorCodeEmptyResponse,
			http.StatusServiceUnavailable,
		)
	}
	if streamErr != nil && !c.Writer.Written() {
		helper.ResetEventStreamHeaders(c)
		return nil, streamErr
	}

	info.StreamStatus.RequireTerminal()
	return accumulator.Finish(), nil
}

func rewriteSGLangResponsesCreatedAt(info *relaycommon.RelayInfo, payload []byte, path string, createdAt dto.IntValue) []byte {
	if info.GetChannelType() != constant.ChannelTypeSGLang {
		return payload
	}
	if !ojson.GetBytes(payload, path).Exists() {
		return payload
	}
	patched, err := ojson.SetBytes(payload, path, int(createdAt))
	if err != nil {
		return payload
	}
	return patched
}
