package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExtractHTML checks that a document is isolated from surrounding prose and
// that an incomplete envelope is rejected.
func TestExtractHTML(t *testing.T) {
	document := "<!doctype html><html><body><p>hi</p></body></html>"
	cases := []struct {
		name string
		text string
		want string
	}{
		{"plain", document, document},
		{"leading prose", "这是结果：\n\n" + document, document},
		{"code fence", "```html\n" + document + "\n```", document},
		{"trailing prose", document + "\n希望有帮助。", document},
		{"missing body", "<!doctype html><html><p>hi</p></html>", ""},
		{"missing close", "<!doctype html><html><body>hi</body>", ""},
		{"no document", "只有一些文字", ""},
		{"empty", "", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := extractHTML(test.text); got != test.want {
				t.Fatalf("extractHTML = %q, want %q", got, test.want)
			}
		})
	}
}

// TestParseGenerationChatStream checks that a completed chat stream yields the
// document and its reported usage.
func TestParseGenerationChatStream(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"<!doctype html><html>"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"<body>ok</body></html>"}}]}`,
		`data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":10}}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	result, err := parseGeneration([]byte(raw), "chat_completions")
	if err != nil {
		t.Fatalf("parseGeneration: %v", err)
	}
	if !strings.Contains(result.HTML, "<body>ok</body>") {
		t.Fatalf("HTML = %q", result.HTML)
	}
	if result.InputTokens == nil || *result.InputTokens != 100 {
		t.Fatal("input tokens were not recorded")
	}
	if result.ReasoningTokens == nil || *result.ReasoningTokens != 10 {
		t.Fatal("reasoning tokens were not recorded")
	}
}

// TestParseGenerationRejectsIncompleteStream checks that a stream without a
// successful terminal event never becomes an artifact.
func TestParseGenerationRejectsIncompleteStream(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"<!doctype html><html><body>ok</body></html>"}}]}`,
		"",
	}, "\n")
	if _, err := parseGeneration([]byte(raw), "chat_completions"); err == nil {
		t.Fatal("an unterminated stream was accepted")
	}
}

// TestParseGenerationResponsesPrefersCompletedObject checks that the completed
// object replaces accumulated deltas instead of duplicating the document.
func TestParseGenerationResponsesPrefersCompletedObject(t *testing.T) {
	document := "<!doctype html><html><body>once</body></html>"
	raw := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"<!doctype html><html><body>once</body></html>"}`,
		fmt.Sprintf(`data: {"type":"response.completed","response":{"status":"completed","error":null,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":10,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5}}}}`, document),
		"",
	}, "\n")
	result, err := parseGeneration([]byte(raw), "responses")
	if err != nil {
		t.Fatalf("parseGeneration: %v", err)
	}
	if strings.Count(result.HTML, "<body>") != 1 {
		t.Fatalf("document was duplicated: %q", result.HTML)
	}
	if result.OutputTokens == nil || *result.OutputTokens != 20 {
		t.Fatal("output tokens were not recorded")
	}
}

// TestParseGenerationRejectsFailure checks that a stream error ends the task.
func TestParseGenerationRejectsFailure(t *testing.T) {
	raw := `data: {"type":"error","error":{"code":"invalid_api_key"}}`
	_, err := parseGeneration([]byte(raw), "responses")
	failure, ok := err.(*remoteFailure)
	if !ok || failure.Category != "auth" || failure.Retryable {
		t.Fatalf("stream failure = %v", err)
	}
}

// TestGenerateRequestsBothProtocols checks the request shape for each protocol
// against a local gateway fixture.
func TestGenerateRequestsBothProtocols(t *testing.T) {
	cases := []struct {
		protocol string
		path     string
		field    string
	}{
		{"responses", "/responses", "max_output_tokens"},
		{"chat_completions", "/chat/completions", "max_completion_tokens"},
	}
	for _, test := range cases {
		t.Run(test.protocol, func(t *testing.T) {
			var receivedPath, authorization string
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				receivedPath = request.URL.Path
				authorization = request.Header.Get("Authorization")
				decodeJSON(t, request.Body, &body)
				writer.Header().Set("Content-Type", "text/event-stream")
				if test.protocol == "chat_completions" {
					_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<!doctype html><html><body>ok</body></html>\"}}]}\n" +
						"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n"))
					return
				}
				_, _ = writer.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"<!doctype html><html><body>ok</body></html>\"}\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n"))
			}))
			defer server.Close()
			client := newDrawingClient()
			result, err := client.generate(context.Background(), &Config{
				Model:    "gpt-6-astra",
				BaseURL:  server.URL,
				Protocol: test.protocol,
			}, "sk-test-key", "画一只猫")
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			if result.HTML == "" {
				t.Fatal("no document was extracted")
			}
			if receivedPath != test.path {
				t.Fatalf("path = %q, want %q", receivedPath, test.path)
			}
			if authorization != "Bearer sk-test-key" {
				t.Fatalf("authorization = %q", authorization)
			}
			if _, ok := body[test.field]; !ok {
				t.Fatalf("payload is missing %q: %v", test.field, body)
			}
			if body["stream"] != true {
				t.Fatal("payload did not request streaming")
			}
		})
	}
}

// TestGenerateRejectsRedirect checks that the stored Key cannot be forwarded.
func TestGenerateRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://elsewhere.example", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newDrawingClient()
	_, err := client.generate(context.Background(), &Config{
		Model: "m", BaseURL: server.URL, Protocol: "responses",
	}, "sk-test-key", "prompt")
	if err == nil {
		t.Fatal("a redirect was followed")
	}
}

// decodeJSON reads a bounded JSON request body into the target.
func decodeJSON(t *testing.T, reader io.Reader, target any) {
	t.Helper()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}
