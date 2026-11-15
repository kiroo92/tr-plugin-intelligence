package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// remote_test.go exercises benchmark submission, result interpretation, and safe
// error classification with local HTTP fixtures. Gateway credentials and arbitrary
// upstream text stay outside public diagnostics, while billing failures retain
// actionable categories and their observed HTTP status.

// TestOutcomeMapping checks that execution state and judgement stay separate.
// A failed or unknown result must never read as a degraded model.
func TestOutcomeMapping(t *testing.T) {
	cases := []struct {
		name   string
		result *remoteTest
		want   string
	}{
		{"nil", nil, VerdictUnknown},
		{"queued", &remoteTest{Status: "queued"}, VerdictPending},
		{"running", &remoteTest{Status: "running"}, VerdictPending},
		{"failed", &remoteTest{Status: "failed"}, VerdictError},
		{"cancelled", &remoteTest{Status: "cancelled"}, VerdictError},
		{"unfamiliar", &remoteTest{Status: "mystery"}, VerdictUnknown},
		{
			"candy passed",
			&remoteTest{Status: "succeeded", Benchmark: BenchmarkCandy, Candy: &remoteCandy{Status: "passed"}},
			VerdictPassed,
		},
		{
			"candy incorrect",
			&remoteTest{Status: "succeeded", Benchmark: BenchmarkCandy, Candy: &remoteCandy{Status: "incorrect"}},
			VerdictFailed,
		},
		{
			"candy error state",
			&remoteTest{Status: "succeeded", Benchmark: BenchmarkCandy, Candy: &remoteCandy{Status: "error"}},
			VerdictError,
		},
		{
			"candy missing result",
			&remoteTest{Status: "succeeded", Benchmark: BenchmarkCandy},
			VerdictUnknown,
		},
		{
			"candy empty error placeholder keeps the verdict",
			&remoteTest{Status: "succeeded", Benchmark: BenchmarkCandy, Candy: &remoteCandy{Status: "passed", Error: json.RawMessage(`{}`)}},
			VerdictPassed,
		},
		{
			"candy reported error wins",
			&remoteTest{Status: "succeeded", Benchmark: BenchmarkCandy, Candy: &remoteCandy{Status: "passed", Error: json.RawMessage(`{"code":"quota"}`)}},
			VerdictError,
		},
		{
			"drawing normal",
			&remoteTest{Status: "succeeded", Benchmark: "html", Assessment: &remoteAssessment{Quality: "normal"}},
			VerdictPassed,
		},
		{
			"drawing degraded",
			&remoteTest{Status: "succeeded", Benchmark: "html", Assessment: &remoteAssessment{Quality: "degraded"}},
			VerdictFailed,
		},
		{
			"drawing suspicious",
			&remoteTest{Status: "succeeded", Benchmark: "html", Assessment: &remoteAssessment{Quality: "suspicious"}},
			VerdictFailed,
		},
		{
			"drawing unknown quality",
			&remoteTest{Status: "succeeded", Benchmark: "html", Assessment: &remoteAssessment{Quality: "unknown"}},
			VerdictUnknown,
		},
		{
			"drawing missing assessment",
			&remoteTest{Status: "succeeded", Benchmark: "html"},
			VerdictUnknown,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := test.result.outcome(); got != test.want {
				t.Fatalf("outcome = %q, want %q", got, test.want)
			}
		})
	}
}

// TestErrorClassification checks that reported failures become allowlisted
// categories and that quota reasons beat a generic rate-limit wrapper.
func TestErrorClassification(t *testing.T) {
	cases := map[string]string{
		`{"error":{"code":"invalid_api_key"}}`:                             "auth",
		`{"error":{"code":"rate_limit"}}`:                                  "rate_limit",
		`{"error":{"message":"额度已用完"}}`:                                    "quota",
		`{"error":{"code":"rate_limit","message":"daily quota exceeded"}}`: "quota",
		`{"error":{"code":"model_not_found"}}`:                             "model",
		`{"error":{"message":"upstream exploded"}}`:                        "",
	}
	for raw, want := range cases {
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("fixture %q is invalid JSON", raw)
		}
		if want == "" {
			continue
		}
		category, _ := inspectRemoteError([]byte(raw))
		if category != want {
			t.Fatalf("%s classified as %q, want %q", raw, category, want)
		}
	}
	if got := categoryFromStatus(401); got != "auth" {
		t.Fatalf("status 401 classified as %q", got)
	}
	if got := categoryFromStatus(402); got != "quota" {
		t.Fatalf("status 402 classified as %q", got)
	}
	if got := categoryFromStatus(503); got != "upstream" {
		t.Fatalf("status 503 classified as %q", got)
	}
}

// TestSubscriptionExpiredClassification reproduces the gateway's HTTP 403 expiry
// envelope under generic permission wrappers. The saved diagnostic must identify
// the expired subscription, retain the status, discard raw text, and prevent retries.
func TestSubscriptionExpiredClassification(t *testing.T) {
	for _, raw := range []string{
		`{"code":403,"reason":"SUBSCRIPTION_EXPIRED","message":"subscription has expired"}`,
		`{"error":{"type":"forbidden","reason":"SUBSCRIPTION_EXPIRED"}}`,
		`{"error":{"code":"permission_denied","message":"subscription has expired; private-details"}}`,
	} {
		response := &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(raw))}
		err := classifyHTTP(response)
		var failure *remoteFailure
		if !errors.As(err, &failure) || failure.Category != "subscription_expired" || failure.Status != http.StatusForbidden {
			t.Fatalf("expiry response classified as %v", err)
		}
		if failure.Retryable || isRetryable(err) {
			t.Fatal("expired subscription must not consume another generation attempt")
		}
		if !strings.Contains(failure.Error(), "订阅已过期") || !strings.Contains(failure.Error(), "HTTP 403") {
			t.Fatalf("unactionable expiry diagnostic: %s", failure.Error())
		}
		if strings.Contains(failure.Error(), "private-details") {
			t.Fatal("upstream details leaked into the safe diagnostic")
		}
	}
}

// TestRemoteFailureMessages checks that the fixed message is used and that a
// status is appended without any upstream text.
func TestRemoteFailureMessages(t *testing.T) {
	failure := &remoteFailure{Category: "quota", Status: 429}
	message := failure.Error()
	if !strings.Contains(message, "额度不足") || !strings.Contains(message, "HTTP 429") {
		t.Fatalf("unexpected message %q", message)
	}
	unknown := &remoteFailure{Category: "not-a-category"}
	if !strings.Contains(unknown.Error(), "未提供可安全展示") {
		t.Fatalf("unknown category fell through to %q", unknown.Error())
	}
}

// TestCreateRewritesDrawingBody checks that an html submission never carries a
// generation credential or generation parameter.
func TestCreateRewritesDrawingBody(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &received)
		if got := request.Header.Get("Idempotency-Key"); got != "html-gallery-test" {
			t.Errorf("idempotency header = %q", got)
		}
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"id":"task_1","benchmark":"html","status":"succeeded","assessment":{"quality":"normal","reason":"ok"}}`))
	}))
	defer server.Close()
	client := newRemoteClient(server.URL)
	result, err := client.create(context.Background(), createRequest{
		Benchmark:       "html",
		BaseURL:         "https://relay.example.com/v1",
		APIKey:          "sk-should-not-leave",
		Model:           "local-model",
		Protocol:        "responses",
		ReasoningEffort: "high",
		ServiceTier:     "priority",
		HTML:            "<!doctype html><html><body>ok</body></html>",
	}, "html-gallery-test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.outcome() != VerdictPassed {
		t.Fatalf("outcome = %q", result.outcome())
	}
	if received["model"] != remoteModel {
		t.Fatalf("model = %v, want %s", received["model"], remoteModel)
	}
	if received["api_key"] == "sk-should-not-leave" {
		t.Fatal("the generation credential was forwarded to the classifier")
	}
	for _, forbidden := range []string{"protocol", "reasoning_effort", "service_tier"} {
		if _, present := received[forbidden]; present {
			t.Fatalf("html creation carried %q", forbidden)
		}
	}
}

// TestCreateSendsCandyCredential checks that candy submissions do carry the
// real gateway address and Key because the tester calls the site itself.
func TestCreateSendsCandyCredential(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &received)
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"id":"candy_1","benchmark":"candy","status":"queued"}`))
	}))
	defer server.Close()
	client := newRemoteClient(server.URL)
	if _, err := client.create(context.Background(), createRequest{
		Benchmark: BenchmarkCandy,
		BaseURL:   "https://relay.example.com/v1",
		APIKey:    "sk-candy-key-value",
		Model:     "gpt-6-astra",
	}, "run-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if received["api_key"] != "sk-candy-key-value" {
		t.Fatalf("candy creation did not carry the Key: %v", received["api_key"])
	}
	if received["protocol"] != "responses" {
		t.Fatalf("candy protocol = %v", received["protocol"])
	}
}

// TestCreateRejectsRedirect checks that a redirect is refused instead of
// forwarding the credential to another origin.
func TestCreateRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://elsewhere.example", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newRemoteClient(server.URL)
	_, err := client.create(context.Background(), createRequest{
		Benchmark: BenchmarkCandy,
		BaseURL:   "https://relay.example.com/v1",
		APIKey:    "sk-candy-key-value",
		Model:     "gpt-6-astra",
	}, "run-1")
	var failure *remoteFailure
	if !asFailure(err, &failure) || failure.Status != http.StatusTemporaryRedirect {
		t.Fatalf("redirect produced %v", err)
	}
}

// TestGetValidatesIdentity checks that a mismatched identifier is rejected.
func TestGetValidatesIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"id":"different","status":"succeeded"}`))
	}))
	defer server.Close()
	client := newRemoteClient(server.URL)
	if _, err := client.get(context.Background(), "task_1"); err == nil {
		t.Fatal("get accepted a response for another task")
	}
	if _, err := client.get(context.Background(), "../escape"); err == nil {
		t.Fatal("get accepted an unsafe identifier")
	}
}

// TestWaitStopsAtDeadline checks that polling gives up instead of blocking.
func TestWaitStopsAtDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"id":"task_1","status":"running"}`))
	}))
	defer server.Close()
	client := newRemoteClient(server.URL)
	client.pollInterval = time.Millisecond
	_, err := client.wait(context.Background(), "task_1", time.Now().Add(30*time.Millisecond))
	var failure *remoteFailure
	if !asFailure(err, &failure) || failure.Category != "timeout" {
		t.Fatalf("wait returned %v", err)
	}
}

// asFailure is a typed assertion helper kept local to the tests.
func asFailure(err error, target **remoteFailure) bool {
	failure, ok := err.(*remoteFailure)
	if ok {
		*target = failure
	}
	return ok
}
