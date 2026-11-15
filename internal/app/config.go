package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"
)

// config.go validates administrator input and the settings shared by the
// scheduler. It also owns the public-address checks that keep a saved gateway
// URL from reaching loopback or private networks.

// maxConfigs bounds how many configurations one installation stores.
const maxConfigs = 200

// settings holds the plugin-level switches. The feature switch hides user
// access and stops new tasks; submitted remote work still finishes.
type settings struct {
	Enabled bool `json:"enabled"`
}

// defaultSettings keeps the feature off until an administrator enables it.
func defaultSettings() settings { return settings{Enabled: false} }

// normalizeInput trims and applies defaults before validation so an empty
// optional field is stored in its canonical form.
func normalizeInput(input *ConfigInput) {
	input.Model = strings.TrimSpace(input.Model)
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.APIKey = strings.TrimSpace(input.APIKey)
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.Protocol = strings.TrimSpace(input.Protocol)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	input.ServiceTier = strings.TrimSpace(input.ServiceTier)
	if input.IntervalMinutes == 0 {
		input.IntervalMinutes = 60
	}
	if input.Protocol == "" {
		input.Protocol = "responses"
	}
}

// validateInput checks one configuration submission. The resolved group name
// comes from the host catalog, never from the raw request body.
func validateInput(input ConfigInput, groupName string) error {
	if input.GroupID <= 0 || strings.TrimSpace(groupName) == "" || utf8.RuneCountInString(groupName) > 60 {
		return errors.New("请选择有效分组")
	}
	if input.Model == "" || len(input.Model) > maxModelBytes {
		return errors.New("请填写有效的模型标识")
	}
	if input.Benchmark != BenchmarkCandy && input.Benchmark != BenchmarkDrawing {
		return errors.New("请选择糖果或画图测试")
	}
	if len(input.Prompt) > maxPromptBytes || (input.Benchmark != BenchmarkDrawing && input.Prompt != "") {
		return errors.New("自定义提示词仅用于画图测试，最多 32 KiB")
	}
	if input.IntervalMinutes < 5 || input.IntervalMinutes > 10080 {
		return errors.New("检测间隔须为 5 至 10080 分钟")
	}
	if input.Protocol != "responses" && input.Protocol != "chat_completions" {
		return errors.New("请求协议仅支持 Responses 或 Chat Completions")
	}
	if input.Benchmark == BenchmarkCandy && input.Protocol != "responses" {
		return errors.New("糖果测试仅支持 Responses 协议")
	}
	if !allowedReasoning(input.Benchmark, input.ReasoningEffort) {
		return errors.New("不支持的推理强度")
	}
	if input.ServiceTier != "" && input.ServiceTier != "priority" && input.ServiceTier != "ultrafast" {
		return errors.New("不支持的服务档位")
	}
	_, err := validateBaseURL(input.BaseURL)
	return err
}

// allowedReasoning lists the reasoning levels each benchmark accepts. Drawing
// stops at high because the classifier compares against that generation mode.
func allowedReasoning(benchmark, effort string) bool {
	switch benchmark {
	case BenchmarkDrawing:
		switch effort {
		case "", "low", "medium", "high":
			return true
		}
		return false
	case BenchmarkCandy:
		switch effort {
		case "", "low", "medium", "high", "xhigh", "max", "ultra":
			return true
		}
		return false
	default:
		return false
	}
}

// validateBaseURL accepts only a public HTTPS gateway origin with an optional
// /v1 path. Credentials, query parameters, fragments, and local addresses are
// rejected because the plugin sends a stored Key to this exact endpoint.
func validateBaseURL(raw string) (string, error) {
	if raw == "" || len(raw) > maxBaseURLBytes {
		return "", errors.New("请填写有效的 Base URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("Base URL 须为公网 HTTPS 地址")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Base URL 不可包含账号密码、查询参数或片段")
	}
	host := parsed.Hostname()
	if host == "" || isLocalHost(host) {
		return "", errors.New("Base URL 不可指向本机或内网地址")
	}
	if address := net.ParseIP(host); address != nil && !isPublicAddress(address) {
		return "", errors.New("Base URL 不可指向内网地址")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

// isLocalHost rejects the names that resolve to the machine itself before any
// DNS lookup happens.
func isLocalHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(host, "."))
	return normalized == "localhost" || strings.HasSuffix(normalized, ".localhost")
}

// isPublicAddress reports whether an address can be reached on the public
// internet. Private, loopback, link-local, multicast, and unspecified ranges
// are all rejected.
func isPublicAddress(address net.IP) bool {
	if address == nil || address.IsUnspecified() || address.IsLoopback() ||
		address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsInterfaceLocalMulticast() || address.IsMulticast() {
		return false
	}
	// IPv4-mapped IPv6 addresses keep their IPv4 classification.
	if v4 := address.To4(); v4 != nil && !address.Equal(v4) {
		return isPublicAddress(v4)
	}
	return true
}

// checkBaseURL validates a gateway URL through the installed policy.
func (plugin *Plugin) checkBaseURL(ctx context.Context, raw string) (string, error) {
	if plugin.baseURLPolicy == nil {
		return validateBaseURL(raw)
	}
	return plugin.baseURLPolicy(ctx, raw)
}

// resolvePolicy validates the saved URL and every address its host resolves to.
// Re-checking before a chargeable request limits DNS rebinding between save and
// execution. A resolver is injectable so tests avoid real lookups.
func (plugin *Plugin) resolvePolicy(ctx context.Context, raw string) (string, error) {
	normalized, err := validateBaseURL(raw)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", errors.New("Base URL 须为公网 HTTPS 地址")
	}
	host := parsed.Hostname()
	if address := net.ParseIP(host); address != nil {
		if !isPublicAddress(address) {
			return "", errors.New("Base URL 不可指向内网地址")
		}
		return normalized, nil
	}
	addresses, err := plugin.lookupIP(host)
	if err != nil || len(addresses) == 0 {
		return "", errors.New("无法解析 Base URL 的主机名")
	}
	for _, address := range addresses {
		if !isPublicAddress(address) {
			return "", fmt.Errorf("Base URL 解析到内网地址 %s，已拒绝", address)
		}
	}
	return normalized, nil
}
