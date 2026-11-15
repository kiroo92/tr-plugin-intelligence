package app

import (
	"errors"
	"regexp"
	"time"
)

// types.go defines the stored configuration and observation shapes together
// with the benchmark limits shared by validation, generation, and assessment.

// Benchmark identifiers select the evaluation task. Candy asks a fixed puzzle
// through the remote tester; drawing generates a document locally and submits
// it for quality classification.
const (
	BenchmarkCandy   = "candy"
	BenchmarkDrawing = "drawing"
)

// Run statuses describe execution, while verdicts describe the judged quality.
// The two stay separate so a failed request can never read as a degraded model.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	VerdictPending = "pending"
	VerdictPassed  = "passed"
	VerdictFailed  = "failed"
	VerdictUnknown = "unknown"
	VerdictError   = "error"
)

// Execution phases order the pipeline for display and recovery. Candy runs the
// model remotely, so it reports a single testing phase.
const (
	PhaseQueued     = "queued"
	PhaseGenerating = "generating"
	PhaseGenerated  = "generated"
	PhaseAssessing  = "assessing"
	PhaseTesting    = "testing"
	PhaseDone       = "complete"
)

// Remote limits mirror the upstream tester's published bounds. They are also
// enforced locally so an oversized document never leaves the plugin.
const (
	maxResponseBytes = 4 << 20
	maxHTMLBytes     = 2 << 20
	maxPromptBytes   = 32 << 10
	maxModelBytes    = 256
	maxBaseURLBytes  = 2048

	// generationMaxOutputTokens bounds one drawing request on either protocol.
	generationMaxOutputTokens = 8192
	// generationAttempts bounds chargeable gateway submissions per task.
	generationAttempts = 3
	// assessmentAttempts bounds classifier submissions per task.
	assessmentAttempts = 5
	// observationWindow bounds how long one task may keep retrying.
	observationWindow = 30 * time.Minute
	// workerCount bounds concurrent model work; each task is long-running.
	workerCount = 2
)

// credentialPattern removes recognizable site credentials from stored text even
// though the configured Key is already known to the worker.
var credentialPattern = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)

var (
	// errNotFound reports a missing configuration or observation.
	errNotFound = errors.New("记录不存在")
	// errInvalid reports a rejected request that the caller can correct.
	errInvalid = errors.New("请求参数无效")
	// errBusy reports an occupied configuration with an active task.
	errBusy = errors.New("当前配置正在检测，请等待本次检测结束")
	// errDisabled reports the plugin-level feature switch being off.
	errDisabled = errors.New("智商测试已关闭")
	// errNoMasterKey reports a missing or unusable credential master key.
	errNoMasterKey = errors.New("凭据主密钥未配置，无法保存或读取 API Key")
)

// Config stores one immutable test identity with its administrator settings.
// The credential itself lives in keyCipher and never enters a response.
type Config struct {
	ID              int64  `json:"id"`
	GroupID         int64  `json:"group_id"`
	GroupName       string `json:"group_name"`
	Model           string `json:"model"`
	Benchmark       string `json:"benchmark"`
	Prompt          string `json:"prompt"`
	BaseURL         string `json:"base_url"`
	Protocol        string `json:"protocol"`
	ReasoningEffort string `json:"reasoning_effort"`
	ServiceTier     string `json:"service_tier"`
	Enabled         bool   `json:"enabled"`
	ScheduleEnabled bool   `json:"schedule_enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
	NextRunAt       int64  `json:"next_run_at"`
	KeySet          bool   `json:"key_set"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`

	// keyID identifies the encrypted credential so its ciphertext cannot be
	// swapped between configurations.
	keyID string
	// keyCipher holds the AES-GCM payload with a random nonce prefix.
	keyCipher string
}

// ConfigInput carries administrator fields for create and edit. An empty Key
// keeps the stored credential; identity fields stay fixed after creation.
type ConfigInput struct {
	GroupID int64 `json:"group_id"`
	// GroupName supplies a manual label when the host catalog does not list the
	// group. When the catalog knows the group, its name takes precedence.
	GroupName       string `json:"group_name"`
	Model           string `json:"model"`
	Benchmark       string `json:"benchmark"`
	Prompt          string `json:"prompt"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	Protocol        string `json:"protocol"`
	ReasoningEffort string `json:"reasoning_effort"`
	ServiceTier     string `json:"service_tier"`
	Enabled         bool   `json:"enabled"`
	ScheduleEnabled bool   `json:"schedule_enabled"`
	IntervalMinutes int    `json:"interval_minutes"`
}

// Run stores one observation. Large fields stay out of list responses: HTML is
// served by a dedicated endpoint and the rest is summarized.
type Run struct {
	ID                string `json:"id"`
	ConfigID          int64  `json:"config_id"`
	GroupID           int64  `json:"group_id"`
	Model             string `json:"model"`
	Benchmark         string `json:"benchmark"`
	Status            string `json:"status"`
	Verdict           string `json:"verdict"`
	Phase             string `json:"phase"`
	Question          string `json:"question,omitempty"`
	Answer            string `json:"answer,omitempty"`
	HTML              string `json:"-"`
	InputTokens       *int64 `json:"input_tokens,omitempty"`
	OutputTokens      *int64 `json:"output_tokens,omitempty"`
	ReasoningTokens   *int64 `json:"reasoning_tokens,omitempty"`
	DurationMS        *int64 `json:"duration_ms,omitempty"`
	FirstTokenMS      *int64 `json:"first_token_ms,omitempty"`
	AssessmentReason  string `json:"assessment_reason,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	HasHTML           bool   `json:"has_html"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	FinishedAt        int64  `json:"finished_at,omitempty"`
	GenerationTries   int    `json:"-"`
	AssessmentTries   int    `json:"-"`
	remoteID          string
	hasRemoteIdentity bool
}

// Judged reports whether the verdict is a definite quality conclusion. Only
// these runs enter history bars and pass rates.
func (run *Run) Judged() bool {
	return run != nil && (run.Verdict == VerdictPassed || run.Verdict == VerdictFailed)
}

// Terminal reports whether the run can no longer change.
func (run *Run) Terminal() bool {
	return run != nil && (run.Status == StatusSucceeded || run.Status == StatusFailed || run.Status == StatusCancelled)
}

// summary strips a run to the fields list views render.
func (run *Run) summary() *Run {
	if run == nil {
		return nil
	}
	copied := *run
	copied.HTML = ""
	copied.Answer = ""
	copied.Question = ""
	return &copied
}
