package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// generate.go requests one drawing document from the configured gateway and
// extracts a complete HTML envelope. Gateway credentials stay on this request;
// the classifier never receives them.
//
// Both supported protocols may answer with buffered JSON or server-sent events.
// A stream must reach a successful terminal state before its text counts as an
// artifact, so a disconnected or truncated response never becomes a document.

var (
	// htmlStartPattern finds the beginning of a document inside model prose.
	htmlStartPattern = regexp.MustCompile(`(?i)<!doctype\s+html\b|<html(?:\s|>)`)
	// htmlEndPattern finds the end of that document.
	htmlEndPattern = regexp.MustCompile(`(?i)</html\s*>`)
	// bodyStartPattern requires a body element in the extracted document.
	bodyStartPattern = regexp.MustCompile(`(?i)<body(?:\s|>)`)
	// bodyEndPattern requires the matching closing body element.
	bodyEndPattern = regexp.MustCompile(`(?i)</body\s*>`)
)

// defaultPrompt supplies a self-contained animation task when the
// administrator leaves the drawing instruction empty.
const defaultPrompt = "请生成可直接运行的单文件HTML，使用内联SVG绘制鹈鹕骑自行车的二维循环动画。" +
	"画面以鹈鹕和自行车为主体，展示清晰的身体结构、踩踏动作和车轮转动，配合协调的背景、配色与层次。" +
	"动画应流畅自然、衔接连续，并适配不同屏幕尺寸。禁止依赖外部资源，只输出完整HTML，不要代码围栏或解释文字。"

// drawingClient issues gateway generation requests.
type drawingClient struct {
	http *http.Client
}

// newDrawingClient builds a client that refuses redirects so the stored Key
// cannot be forwarded to another origin.
func newDrawingClient() *drawingClient {
	return &drawingClient{http: &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// drawingResult is one completed generation with its measurements.
type drawingResult struct {
	HTML            string
	InputTokens     *int64
	OutputTokens    *int64
	ReasoningTokens *int64
	DurationMS      int64
	FirstTokenMS    *int64
}

// generate requests one document. The prompt is required; an empty prompt is
// replaced by the caller with the default instruction.
func (client *drawingClient) generate(ctx context.Context, config *Config, key, prompt string) (*drawingResult, error) {
	path := "/responses"
	payload := map[string]any{
		"model":             config.Model,
		"stream":            true,
		"max_output_tokens": generationMaxOutputTokens,
		"store":             false,
	}
	if config.Protocol == "chat_completions" {
		path = "/chat/completions"
		payload = map[string]any{
			"model":                 config.Model,
			"stream":                true,
			"messages":              []map[string]string{{"role": "user", "content": prompt}},
			"max_completion_tokens": generationMaxOutputTokens,
			"stream_options":        map[string]bool{"include_usage": true},
		}
		if config.ReasoningEffort != "" {
			payload["reasoning_effort"] = config.ReasoningEffort
		}
	} else {
		payload["input"] = prompt
		if config.ReasoningEffort != "" {
			payload["reasoning"] = map[string]string{"effort": config.ReasoningEffort}
		}
	}
	if config.ServiceTier != "" {
		payload["service_tier"] = config.ServiceTier
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(config.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream, application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	// A transport replay could spend the same Key's quota again.
	request.GetBody = nil
	started := time.Now()
	response, err := client.http.Do(request)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, classifyHTTP(response)
	}
	raw, firstToken, err := readGenerationBody(response.Body, started)
	if err != nil {
		return nil, err
	}
	result, err := parseGeneration(raw, config.Protocol)
	if err != nil {
		return nil, err
	}
	result.DurationMS = time.Since(started).Milliseconds()
	result.FirstTokenMS = firstToken
	return result, nil
}

// readGenerationBody buffers a bounded response while recording the arrival of
// the first nonempty content delta. Heartbeats and lifecycle events do not
// count, so a slow first token stays distinguishable from total duration.
func readGenerationBody(source io.Reader, started time.Time) ([]byte, *int64, error) {
	reader := bufio.NewReader(io.LimitReader(source, maxResponseBytes+1))
	var body bytes.Buffer
	var firstToken *int64
	for {
		line, err := reader.ReadString('\n')
		_, _ = body.WriteString(line)
		if body.Len() > maxResponseBytes {
			return nil, nil, &remoteFailure{Category: "remote_error"}
		}
		if firstToken == nil && strings.HasPrefix(line, "data:") {
			if contentDelta(strings.TrimSpace(strings.TrimPrefix(line, "data:"))) {
				elapsed := time.Since(started).Milliseconds()
				firstToken = &elapsed
			}
		}
		if err == io.EOF {
			return body.Bytes(), firstToken, nil
		}
		if err != nil {
			return nil, nil, &remoteFailure{Category: "transport", Retryable: true}
		}
	}
}

// contentDelta reports whether one event line carries nonempty model text or
// reasoning text on either protocol.
func contentDelta(data string) bool {
	if data == "" || data == "[DONE]" {
		return false
	}
	var event map[string]any
	if json.Unmarshal([]byte(data), &event) != nil {
		return false
	}
	if choices, ok := event["choices"].([]any); ok {
		for _, item := range choices {
			choice, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if index, ok := choice["index"].(float64); ok && index != 0 {
				continue
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"content", "reasoning_content", "reasoning"} {
				if text, ok := delta[field].(string); ok && text != "" {
					return true
				}
			}
		}
		return false
	}
	switch event["type"] {
	case "response.output_text.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.refusal.delta":
		text, _ := event["delta"].(string)
		return text != ""
	}
	return false
}

// parseGeneration decodes complete JSON or a terminated event stream. A stream
// needs a successful terminal event, and a buffered object needs a completed
// status, before its text can be extracted.
func parseGeneration(raw []byte, protocol string) (*drawingResult, error) {
	var text strings.Builder
	var usage map[string]any
	complete := false
	if json.Valid(raw) {
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, &remoteFailure{Category: "remote_error"}
		}
		if failure, ok := data["error"]; ok && failure != nil {
			return nil, failureToRemote(failure)
		}
		usage, _ = data["usage"].(map[string]any)
		if protocol == "chat_completions" {
			text.WriteString(chatMessage(data))
			complete = chatFinished(data)
		} else {
			text.WriteString(responsesOutput(data))
			complete = data["status"] == "completed"
			if data["error"] != nil {
				complete = false
			}
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Buffer(make([]byte, 4096), maxResponseBytes)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return nil, &remoteFailure{Category: "remote_error"}
			}
			if failure := event["error"]; failure != nil {
				return nil, failureToRemote(failure)
			}
			if protocol == "chat_completions" {
				appendChatDelta(&text, event)
				if finished, ok := chatFinishedState(event); ok {
					complete = finished
				}
				if reported, ok := event["usage"].(map[string]any); ok {
					usage = reported
				}
				continue
			}
			switch event["type"] {
			case "response.output_text.delta":
				if delta, ok := event["delta"].(string); ok {
					text.WriteString(delta)
				}
			case "response.completed":
				response, _ := event["response"].(map[string]any)
				complete = response != nil && response["status"] == "completed" && response["error"] == nil
				if reported, ok := response["usage"].(map[string]any); ok {
					usage = reported
				}
				if output := responsesOutput(response); output != "" {
					// The completed object repeats text already streamed as deltas.
					text.Reset()
					text.WriteString(output)
				}
			case "response.failed":
				response, _ := event["response"].(map[string]any)
				if response != nil {
					return nil, failureToRemote(response["error"])
				}
				return nil, &remoteFailure{Category: "remote_error"}
			case "error":
				return nil, failureToRemote(event)
			case "response.incomplete":
				return nil, &remoteFailure{Category: "remote_error"}
			}
		}
		if scanner.Err() != nil {
			return nil, &remoteFailure{Category: "transport", Retryable: true}
		}
	}
	if !complete {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	html := extractHTML(text.String())
	if html == "" {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	if len(html) > maxHTMLBytes {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	result := &drawingResult{HTML: html}
	if protocol == "chat_completions" {
		result.InputTokens = numericToken(usage["prompt_tokens"])
		result.OutputTokens = numericToken(usage["completion_tokens"])
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			result.ReasoningTokens = numericToken(details["reasoning_tokens"])
		}
	} else {
		result.InputTokens = numericToken(usage["input_tokens"])
		result.OutputTokens = numericToken(usage["output_tokens"])
		if details, ok := usage["output_tokens_details"].(map[string]any); ok {
			result.ReasoningTokens = numericToken(details["reasoning_tokens"])
		}
	}
	return result, nil
}

// chatMessage reads the assistant text of a buffered chat completion.
func chatMessage(data map[string]any) string {
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return ""
	}
	message, _ := choice["message"].(map[string]any)
	content, _ := message["content"].(string)
	return content
}

// chatFinished reports whether a buffered chat completion ended normally.
func chatFinished(data map[string]any) bool {
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	return choice != nil && choice["finish_reason"] == "stop"
}

// appendChatDelta adds streamed content from one chat event.
func appendChatDelta(text *strings.Builder, event map[string]any) {
	choices, _ := event["choices"].([]any)
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if index, ok := choice["index"].(float64); ok && index != 0 {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			text.WriteString(content)
		}
	}
}

// chatFinishedState reports the finish state carried by a chat event.
func chatFinishedState(event map[string]any) (bool, bool) {
	choices, _ := event["choices"].([]any)
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			return reason == "stop", true
		}
	}
	return false, false
}

// responsesOutput joins assistant output text from a Responses object.
func responsesOutput(data map[string]any) string {
	var text strings.Builder
	output, _ := data["output"].([]any)
	for _, item := range output {
		entry, ok := item.(map[string]any)
		if !ok || entry["type"] != "message" || entry["role"] != "assistant" {
			continue
		}
		content, _ := entry["content"].([]any)
		for _, part := range content {
			piece, ok := part.(map[string]any)
			if !ok || piece["type"] != "output_text" {
				continue
			}
			if value, ok := piece["text"].(string); ok {
				text.WriteString(value)
			}
		}
	}
	return text.String()
}

// failureToRemote classifies a reported failure without keeping its text.
func failureToRemote(value any) error {
	category := "remote_error"
	switch typed := value.(type) {
	case string:
		if detected := categoryFromMessage(typed); detected != "" {
			category = detected
		}
	case map[string]any:
		for _, field := range []string{"code", "type"} {
			if text, ok := typed[field].(string); ok {
				if detected := categoryFromMessage(text); detected != "" {
					category = detected
					break
				}
			}
		}
		if category == "remote_error" {
			if text, ok := typed["message"].(string); ok {
				if detected := categoryFromMessage(text); detected != "" {
					category = detected
				}
			}
		}
	}
	return &remoteFailure{Category: category, Retryable: retryableCategory(category)}
}

// numericToken keeps nonnegative integer measurements and ignores the rest so
// an absent value stays distinguishable from a reported zero.
func numericToken(value any) *int64 {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(int64(number)) {
		return nil
	}
	result := int64(number)
	return &result
}

// extractHTML isolates a complete document including its body element. Leading
// prose and code fences are removed, and an incomplete envelope returns empty.
func extractHTML(text string) string {
	start := htmlStartPattern.FindStringIndex(text)
	if start == nil {
		return ""
	}
	text = text[start[0]:]
	end := htmlEndPattern.FindStringIndex(text)
	if end == nil {
		return ""
	}
	document := strings.TrimSpace(text[:end[1]])
	if !bodyStartPattern.MatchString(document) || !bodyEndPattern.MatchString(document) {
		return ""
	}
	return document
}

// isRetryable reports whether a generation failure may succeed on a new
// attempt. Authentication, permission, quota, model, and parameter errors are
// permanent for this configuration.
func isRetryable(err error) bool {
	var failure *remoteFailure
	if errors.As(err, &failure) {
		return failure.Retryable
	}
	return false
}
