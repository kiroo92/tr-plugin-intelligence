package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// remote.go talks to the Manxue tester. Creation submits a benchmark, polling
// reads its progress, and both responses are reduced to allowlisted categories
// before any text reaches storage or the panel.
//
// Candy creation carries the real gateway URL and Key because the remote
// service calls the site itself. Drawing creation carries a locally generated
// document with placeholder credentials because nothing is called upstream.

// remoteBaseURL is the fixed tester origin; tests replace it with a loopback
// fixture through newRemoteClient.
const remoteBaseURL = "https://manxue.ai"

// remoteModel is the only model identifier the html benchmark accepts.
const remoteModel = "gpt-6-astra"

var (
	// remoteTaskPattern bounds remote identifiers to a path-safe alphabet.
	remoteTaskPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	// idempotencyPattern bounds the idempotency header value.
	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// statusPattern recognises an explicit HTTP status inside a remote message.
	statusPattern = regexp.MustCompile(`(?i)\b(?:http(?:/[0-9](?:\.[0-9])?)?(?:\s+(?:error|status(?:\s+code)?))?|status(?:[_ ]+code)?)(?:\s*[:=]\s*|\s+)([45][0-9]{2})\b`)
)

// remoteErrorMessages binds each category to one fixed, safe sentence.
var remoteErrorMessages = map[string]string{
	"auth":            "模型接口鉴权失败，请检查检测 Key",
	"permission":      "模型接口拒绝访问，请检查 Key 和模型权限",
	"quota":           "模型接口余额或额度不足",
	"rate_limit":      "模型接口请求受限，请稍后重试",
	"timeout":         "模型接口请求超时",
	"invalid_request": "模型接口不接受检测请求，请检查模型、协议和参数",
	"model":           "模型不存在或不可用，请检查模型名称和分组支持范围",
	"transport":       "检测服务无法连接模型接口",
	"upstream":        "模型接口服务异常，请稍后重试",
	"remote_error":    "检测服务报告执行失败，未提供可安全展示的具体原因",
}

// remoteFailure carries a stable category and optional HTTP status. Its message
// is always generated locally from the allowlist.
type remoteFailure struct {
	Category  string
	Status    int
	Retryable bool
}

// Error renders the fixed message for this category.
func (failure *remoteFailure) Error() string {
	message, ok := remoteErrorMessages[failure.Category]
	if !ok {
		message = remoteErrorMessages["remote_error"]
	}
	if failure.Status >= 400 && failure.Status <= 599 {
		message += "（HTTP " + strconv.Itoa(failure.Status) + "）"
	}
	return message
}

// remoteAssessment is the classifier's verdict for one drawing.
type remoteAssessment struct {
	Quality string `json:"quality"`
	Label   string `json:"label"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

// remoteCandy carries the puzzle result and its measurements.
type remoteCandy struct {
	Status          string          `json:"status"`
	Question        string          `json:"question"`
	Answer          string          `json:"answer"`
	DurationMS      *int64          `json:"duration_ms"`
	InputTokens     *int64          `json:"input_tokens"`
	OutputTokens    *int64          `json:"output_tokens"`
	ReasoningTokens *int64          `json:"reasoning_tokens"`
	Error           json.RawMessage `json:"error"`
}

// remoteDrawing carries a remote-generated document when the tester produced
// one. Locally generated documents never use these fields.
type remoteDrawing struct {
	HTML string `json:"html"`
}

// remoteTest is one decoded task snapshot.
type remoteTest struct {
	ID         string            `json:"id"`
	Benchmark  string            `json:"benchmark"`
	Status     string            `json:"status"`
	Phase      string            `json:"phase"`
	Model      string            `json:"model"`
	Candy      *remoteCandy      `json:"candy"`
	Result     *remoteDrawing    `json:"result"`
	Assessment *remoteAssessment `json:"assessment"`
	Error      json.RawMessage   `json:"error"`
}

// createRequest is the creation body before benchmark-specific rewriting.
type createRequest struct {
	Benchmark       string `json:"benchmark"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	Model           string `json:"model"`
	Protocol        string `json:"protocol,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`
	HTML            string `json:"html,omitempty"`
}

// remoteClient submits and polls tasks. Redirects and request replay are
// disabled because a repeated creation can spend the Key's quota twice.
type remoteClient struct {
	baseURL      string
	http         *http.Client
	pollInterval time.Duration
	now          func() time.Time
}

// newRemoteClient builds a client for the fixed origin. The base URL is
// supplied so tests can point at a loopback fixture.
func newRemoteClient(baseURL string) *remoteClient {
	return &remoteClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		pollInterval: 3 * time.Second,
		now:          time.Now,
	}
}

// create submits one task. Drawing submissions are rewritten so no generation
// credential or parameter leaves the plugin.
func (client *remoteClient) create(ctx context.Context, input createRequest, idempotency string) (*remoteTest, error) {
	if input.Benchmark != BenchmarkCandy && input.Benchmark != "html" {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	if input.Benchmark == "html" {
		if strings.TrimSpace(input.HTML) == "" || len(input.HTML) > maxHTMLBytes {
			return nil, &remoteFailure{Category: "invalid_request"}
		}
		// The html benchmark validates these fields but never calls upstream.
		input = createRequest{
			Benchmark: "html",
			BaseURL:   "https://api.example.com/v1",
			APIKey:    "html-classification-placeholder",
			Model:     remoteModel,
			HTML:      input.HTML,
		}
	} else {
		if input.Protocol == "" {
			input.Protocol = "responses"
		}
		if input.Protocol != "responses" {
			return nil, &remoteFailure{Category: "invalid_request"}
		}
		if strings.TrimSpace(input.APIKey) == "" || strings.TrimSpace(input.Model) == "" {
			return nil, &remoteFailure{Category: "invalid_request"}
		}
	}
	if idempotency != "" && !idempotencyPattern.MatchString(idempotency) {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/api/v1/tests", bytes.NewReader(body))
	if err != nil {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	// A transport-level replay could repeat an accepted, chargeable creation.
	request.GetBody = nil
	response, err := client.http.Do(request)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer func() { _ = response.Body.Close() }()
	accepted := response.StatusCode == http.StatusAccepted ||
		(input.Benchmark == "html" && response.StatusCode >= 200 && response.StatusCode < 300)
	if !accepted {
		return nil, classifyHTTP(response)
	}
	return decodeTest(response.Body, input.Benchmark)
}

// get reads one task snapshot.
func (client *remoteClient) get(ctx context.Context, id string) (*remoteTest, error) {
	if !remoteTaskPattern.MatchString(id) {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/api/v1/tests/"+id, nil)
	if err != nil {
		return nil, &remoteFailure{Category: "invalid_request"}
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, classifyHTTP(response)
	}
	result, err := decodeTest(response.Body, "")
	if err != nil {
		return nil, err
	}
	if result.ID != "" && result.ID != id {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	result.ID = id
	return result, nil
}

// wait polls until the task reaches a terminal state or the deadline passes.
// Transient failures back off and retry; permanent ones return at once.
func (client *remoteClient) wait(ctx context.Context, id string, deadline time.Time) (*remoteTest, error) {
	delay := client.pollInterval
	for {
		result, err := client.get(ctx, id)
		if err == nil {
			if isTerminalStatus(result.Status) {
				return result, nil
			}
			if client.now().After(deadline) {
				return result, &remoteFailure{Category: "timeout"}
			}
		} else {
			var failure *remoteFailure
			if !errors.As(err, &failure) || !failure.Retryable {
				return nil, err
			}
			if client.now().After(deadline) {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay += time.Second
		}
	}
}

// isTerminalStatus reports whether polling can stop.
func isTerminalStatus(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}

// decodeTest reads a bounded response and validates its identity fields.
func decodeTest(body io.Reader, benchmark string) (*remoteTest, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, &remoteFailure{Category: "transport", Retryable: true}
	}
	if len(raw) > maxResponseBytes {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	var result remoteTest
	if err := json.Unmarshal(raw, &result); err != nil || result.Status == "" {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	if result.ID != "" && !remoteTaskPattern.MatchString(result.ID) {
		return nil, &remoteFailure{Category: "remote_error"}
	}
	if benchmark != "" {
		if result.Benchmark != "" && result.Benchmark != benchmark {
			return nil, &remoteFailure{Category: "remote_error"}
		}
		if result.Benchmark == "" {
			result.Benchmark = benchmark
		}
	}
	return &result, nil
}

// classifyHTTP turns a non-success response into a category. The response body
// is inspected for an allowlisted code or an explicit status, then discarded.
func classifyHTTP(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	category, message := "", string(raw)
	if len(raw) <= maxResponseBytes && json.Valid(raw) {
		category, message = inspectRemoteError(raw)
	}
	if category == "" {
		category = categoryFromMessage(message)
	}
	if category == "" {
		category = categoryFromStatus(response.StatusCode)
	}
	return &remoteFailure{Category: category, Status: response.StatusCode, Retryable: retryableCategory(category)}
}

// inspectRemoteError walks the known error envelope. Suppliers nest the useful
// fields one level down under "error", so both levels are inspected.
func inspectRemoteError(raw []byte) (string, string) {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return "", string(raw)
	}
	levels := []map[string]any{payload}
	if nested, ok := payload["error"].(map[string]any); ok {
		levels = append(levels, nested)
	}
	var codes []string
	var messages []string
	for _, level := range levels {
		for _, field := range []string{"code", "type", "status"} {
			if value, ok := level[field].(string); ok {
				codes = append(codes, value)
			}
		}
		for _, field := range []string{"message", "detail"} {
			if value, ok := level[field].(string); ok {
				messages = append(messages, value)
			}
		}
	}
	if text, ok := payload["error"].(string); ok {
		messages = append(messages, text)
	}
	// A quota reason outranks a generic rate-limit wrapper, so every message is
	// inspected before the reported codes decide the category.
	for _, message := range messages {
		if category := categoryFromMessage(message); category == "quota" {
			return category, strings.Join(messages, " ")
		}
	}
	for _, code := range codes {
		if category := categoryFromMessage(code); category != "" {
			return category, strings.Join(messages, " ")
		}
	}
	// A supplier may report only a human-readable message; classify it too, but
	// keep the text local so it never reaches storage.
	for _, message := range messages {
		if category := categoryFromMessage(message); category != "" {
			return category, strings.Join(messages, " ")
		}
	}
	return "", strings.Join(messages, " ")
}

// categoryFromMessage maps known phrases and codes to a category.
func categoryFromMessage(value string) string {
	text := strings.ToLower(value)
	switch {
	case strings.TrimSpace(text) == "":
		return ""
	case containsAny(text, "quota", "insufficient_quota", "billing", "余额", "额度"):
		return "quota"
	case containsAny(text, "rate_limit", "rate limit", "too many requests", "限流", "频率"):
		return "rate_limit"
	case containsAny(text, "invalid_api_key", "invalid api key", "incorrect api key", "authentication", "unauthorized", "鉴权", "认证失败"):
		return "auth"
	case containsAny(text, "permission", "forbidden", "权限"):
		return "permission"
	case containsAny(text, "timeout", "timed out", "超时"):
		return "timeout"
	case containsAny(text, "model_not_found", "model not found", "does not exist", "模型不存在"):
		return "model"
	case containsAny(text, "invalid_request", "invalid request", "bad request", "参数"):
		return "invalid_request"
	}
	return ""
}

// containsAny reports whether the text contains one of the needles.
func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// categoryFromStatus maps an HTTP status to a category when nothing more
// specific was reported.
func categoryFromStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "auth"
	case http.StatusForbidden:
		return "permission"
	case http.StatusPaymentRequired:
		return "quota"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "timeout"
	case http.StatusTooManyRequests:
		return "rate_limit"
	}
	switch {
	case status >= 500:
		return "upstream"
	case status >= 400:
		return "invalid_request"
	default:
		return "remote_error"
	}
}

// retryableCategory reports whether a failure may succeed on a later attempt.
func retryableCategory(category string) bool {
	switch category {
	case "rate_limit", "timeout", "transport", "upstream":
		return true
	default:
		return false
	}
}

// classifyTransport maps a transport error without keeping URL-bearing text.
func classifyTransport(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &remoteFailure{Category: "timeout", Retryable: true}
	}
	return &remoteFailure{Category: "transport", Retryable: true}
}

// outcome combines execution state with the benchmark verdict. Pending work
// stays pending, and a completed execution still needs a recognized judgement
// before it can count as passed or failed.
func (result *remoteTest) outcome() string {
	if result == nil {
		return VerdictUnknown
	}
	switch result.Status {
	case "queued", "pending", "running":
		return VerdictPending
	case "failed", "cancelled":
		return VerdictError
	}
	if result.Status != "succeeded" {
		return VerdictUnknown
	}
	if presentRemoteError(result.Error) {
		return VerdictError
	}
	switch result.Benchmark {
	case BenchmarkCandy:
		if result.Candy == nil {
			return VerdictUnknown
		}
		if presentRemoteError(result.Candy.Error) {
			return VerdictError
		}
		switch result.Candy.Status {
		case "passed":
			return VerdictPassed
		case "incorrect":
			return VerdictFailed
		case "error":
			return VerdictError
		}
	case "html", "pelican", BenchmarkDrawing:
		if result.Assessment == nil {
			return VerdictUnknown
		}
		switch result.Assessment.Quality {
		case "normal":
			return VerdictPassed
		case "degraded", "suspicious":
			return VerdictFailed
		}
	}
	return VerdictUnknown
}

// presentRemoteError distinguishes an empty placeholder from a real error. An
// empty object or empty string is not an error.
func presentRemoteError(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return nonEmptyValue(value, 0)
}

// nonEmptyValue walks a bounded value looking for meaningful content.
func nonEmptyValue(value any, depth int) bool {
	if depth > 8 {
		return true
	}
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		for _, item := range typed {
			if nonEmptyValue(item, depth+1) {
				return true
			}
		}
		return false
	case []any:
		for _, item := range typed {
			if nonEmptyValue(item, depth+1) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// remoteStatus extracts an explicit HTTP status from a reported message so the
// panel can show a safe code without the original text.
func remoteStatus(message string) int {
	match := statusPattern.FindStringSubmatch(message)
	if len(match) < 2 {
		return 0
	}
	status, _ := strconv.Atoi(match[1])
	return status
}

// remoteErrorCategory classifies a reported message for a stored observation.
func remoteErrorCategory(message string) string {
	if category := categoryFromMessage(message); category != "" {
		return category
	}
	if status := remoteStatus(message); status != 0 {
		return categoryFromStatus(status)
	}
	return ""
}

// validateRemoteURL checks the fixed tester origin shape. Plain HTTP is allowed
// only for loopback fixtures so tests need no TLS material.
func validateRemoteURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("检测服务地址无效")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("检测服务地址不可包含路径")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return nil
	}
	return errors.New("检测服务地址须使用 HTTPS")
}
