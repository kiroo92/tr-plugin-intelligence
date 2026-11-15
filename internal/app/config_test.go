package app

import (
	"net"
	"testing"
)

// TestValidateBaseURL checks the public-address rules that protect the stored
// Key from being sent to a local or credential-bearing endpoint.
func TestValidateBaseURL(t *testing.T) {
	accepted := []string{
		"https://relay.example.com/v1",
		"https://relay.example.com/v1/",
		"https://relay.example.com",
		"https://api.example.co.uk/v1",
	}
	for _, value := range accepted {
		if _, err := validateBaseURL(value); err != nil {
			t.Fatalf("validateBaseURL(%q) = %v", value, err)
		}
	}
	rejected := []string{
		"",
		"http://relay.example.com/v1",
		"https://user:pass@relay.example.com/v1",
		"https://relay.example.com/v1?key=1",
		"https://relay.example.com/v1#frag",
		"https://localhost/v1",
		"https://api.localhost/v1",
		"https://127.0.0.1/v1",
		"https://10.0.0.5/v1",
		"https://192.168.1.7/v1",
		"https://169.254.169.254/v1",
		"https://[::1]/v1",
		"https://[fd00::1]/v1",
		"relay.example.com/v1",
	}
	for _, value := range rejected {
		if _, err := validateBaseURL(value); err == nil {
			t.Fatalf("validateBaseURL(%q) was accepted", value)
		}
	}
}

// TestValidateBaseURLTrimsTrailingSlash checks that the stored form has no
// trailing slash so request paths do not double up.
func TestValidateBaseURLTrimsTrailingSlash(t *testing.T) {
	normalized, err := validateBaseURL("https://relay.example.com/v1/")
	if err != nil {
		t.Fatalf("validateBaseURL: %v", err)
	}
	if normalized != "https://relay.example.com/v1" {
		t.Fatalf("normalized = %q", normalized)
	}
}

// TestValidateInputRules checks the benchmark-specific protocol, reasoning, and
// prompt combinations.
func TestValidateInputRules(t *testing.T) {
	base := ConfigInput{
		GroupID:         1,
		Model:           "gpt-6-astra",
		Benchmark:       BenchmarkCandy,
		BaseURL:         "https://relay.example.com/v1",
		Protocol:        "responses",
		ReasoningEffort: "max",
		IntervalMinutes: 60,
	}
	if err := validateInput(base, "default"); err != nil {
		t.Fatalf("baseline candy configuration was rejected: %v", err)
	}

	candyChat := base
	candyChat.Protocol = "chat_completions"
	if err := validateInput(candyChat, "default"); err == nil {
		t.Fatal("candy accepted chat completions")
	}

	candyPrompt := base
	candyPrompt.Prompt = "画一只猫"
	if err := validateInput(candyPrompt, "default"); err == nil {
		t.Fatal("candy accepted a drawing prompt")
	}

	drawingUltra := ConfigInput{
		GroupID: 1, Model: "gpt-6-astra", Benchmark: BenchmarkDrawing,
		BaseURL: "https://relay.example.com/v1", Protocol: "chat_completions",
		ReasoningEffort: "ultra", IntervalMinutes: 60,
	}
	if err := validateInput(drawingUltra, "default"); err == nil {
		t.Fatal("drawing accepted ultra reasoning")
	}

	drawingHigh := drawingUltra
	drawingHigh.ReasoningEffort = "high"
	if err := validateInput(drawingHigh, "default"); err != nil {
		t.Fatalf("drawing with high reasoning was rejected: %v", err)
	}

	shortInterval := base
	shortInterval.IntervalMinutes = 4
	if err := validateInput(shortInterval, "default"); err == nil {
		t.Fatal("an interval below five minutes was accepted")
	}

	missingGroup := base
	missingGroup.GroupID = 0
	if err := validateInput(missingGroup, "default"); err == nil {
		t.Fatal("a missing group was accepted")
	}

	blankName := base
	if err := validateInput(blankName, "  "); err == nil {
		t.Fatal("a blank group name was accepted")
	}
}

// TestNormalizeInputDefaults checks that optional fields land in canonical form.
func TestNormalizeInputDefaults(t *testing.T) {
	input := ConfigInput{Model: "  gpt-6-astra  ", BaseURL: " https://relay.example.com/v1 "}
	normalizeInput(&input)
	if input.Model != "gpt-6-astra" {
		t.Fatalf("model = %q", input.Model)
	}
	if input.IntervalMinutes != 60 {
		t.Fatalf("interval = %d", input.IntervalMinutes)
	}
	if input.Protocol != "responses" {
		t.Fatalf("protocol = %q", input.Protocol)
	}
	// A drawing prompt keeps its surrounding whitespace removed.
	spaced := ConfigInput{Prompt: "  画猫  "}
	normalizeInput(&spaced)
	if spaced.Prompt != "画猫" {
		t.Fatalf("prompt = %q", spaced.Prompt)
	}
}

// TestAllowedReasoning checks the accepted reasoning sets.
func TestAllowedReasoning(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high"} {
		if !allowedReasoning(BenchmarkDrawing, effort) {
			t.Fatalf("drawing rejected %q", effort)
		}
	}
	for _, effort := range []string{"xhigh", "max", "ultra"} {
		if allowedReasoning(BenchmarkDrawing, effort) {
			t.Fatalf("drawing accepted %q", effort)
		}
		if !allowedReasoning(BenchmarkCandy, effort) {
			t.Fatalf("candy rejected %q", effort)
		}
	}
	if allowedReasoning(BenchmarkCandy, "extreme") {
		t.Fatal("candy accepted an unknown effort")
	}
}

// TestIsPublicAddress checks the address classification used by the policy.
func TestIsPublicAddress(t *testing.T) {
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, value := range public {
		if !isPublicAddress(parseIP(t, value)) {
			t.Fatalf("%s was classified as private", value)
		}
	}
	private := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.0.1", "169.254.1.1",
		"0.0.0.0", "224.0.0.1", "::1", "fd00::1", "fe80::1", "::",
	}
	for _, value := range private {
		if isPublicAddress(parseIP(t, value)) {
			t.Fatalf("%s was classified as public", value)
		}
	}
}

// parseIP parses a fixture address or fails the test.
func parseIP(t *testing.T, value string) net.IP {
	t.Helper()
	address := net.ParseIP(value)
	if address == nil {
		t.Fatalf("fixture address %q is invalid", value)
	}
	return address
}
