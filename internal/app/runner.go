package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// runner.go claims queued observations and drives them to a terminal state.
// The scheduled task only enqueues due configurations so a host task deadline
// never interrupts a model request that is already running.

// scheduledTask is the manifest-declared task that enqueues due configurations.
const scheduledTask = "tick"

// RunTask executes one scheduled occurrence. Only the tick task is declared, and
// it returns as soon as due configurations are queued.
func (plugin *Plugin) RunTask(ctx context.Context, name string, _ time.Time) error {
	if name != scheduledTask {
		return nil
	}
	return plugin.tick(ctx)
}

// tick enqueues every configuration whose schedule is due. The next run time is
// computed from the previously planned instant and advanced past now, so a long
// outage does not create a burst of catch-up tasks.
func (plugin *Plugin) tick(ctx context.Context) error {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		return nil
	}
	if !plugin.featureEnabled(ctx) {
		return nil
	}
	now := plugin.now().Unix()
	configs, err := plugin.dueConfigs(ctx, now)
	if err != nil {
		return err
	}
	for _, config := range configs {
		next := plugin.nextScheduled(config, now)
		if _, err := plugin.enqueueScheduled(ctx, config, next); err != nil {
			plugin.logger.Warn("enqueue scheduled check failed", "config", config.ID, "error", err)
		}
	}
	return nil
}

// nextScheduled advances the planned time past now without drifting the
// interval forward from the late execution.
func (plugin *Plugin) nextScheduled(config *Config, now int64) int64 {
	base := config.NextRunAt
	if base <= 0 {
		base = now
	}
	next := nextRun(time.Unix(base, 0), config.IntervalMinutes)
	for next <= now {
		next = nextRun(time.Unix(next, 0), config.IntervalMinutes)
	}
	return next
}

// worker claims observations until the process stops. Two workers bound
// concurrent model work while keeping queue latency low.
func (plugin *Plugin) worker(ctx context.Context) {
	defer plugin.workers.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		run, err := plugin.claim(ctx)
		if err != nil {
			plugin.logger.Warn("claim check failed", "error", err)
			if !sleepContext(ctx, 5*time.Second) {
				return
			}
			continue
		}
		if run == nil {
			if !sleepContext(ctx, 2*time.Second) {
				return
			}
			continue
		}
		plugin.execute(ctx, run)
	}
}

// claim takes the oldest queued observation under the shared database lock.
func (plugin *Plugin) claim(ctx context.Context) (*Run, error) {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		return nil, nil
	}
	return plugin.claimRun(ctx)
}

// execute drives one observation to completion. Every exit path persists a
// terminal state so a task never stays running after the worker returns.
func (plugin *Plugin) execute(ctx context.Context, run *Run) {
	// Work continues under a bounded context so a shutdown drains the current
	// request instead of leaving an ambiguous state.
	config, err := plugin.configFor(ctx, run.ConfigID)
	if err != nil {
		plugin.finish(ctx, run, StatusFailed, VerdictError, "config_missing", "检测配置已删除，本次检测结束")
		return
	}
	if !plugin.featureEnabled(ctx) {
		plugin.finish(ctx, run, StatusFailed, VerdictError, "disabled", "智商测试已关闭，本次尚未提交")
		return
	}
	key, err := plugin.credential(config)
	if err != nil {
		plugin.finish(ctx, run, StatusFailed, VerdictError, "credential", err.Error())
		return
	}
	if _, err := plugin.checkBaseURL(ctx, config.BaseURL); err != nil {
		plugin.finish(ctx, run, StatusFailed, VerdictError, "base_url", err.Error())
		return
	}
	switch config.Benchmark {
	case BenchmarkCandy:
		plugin.runCandy(ctx, run, config, key)
	case BenchmarkDrawing:
		plugin.runDrawing(ctx, run, config, key)
	default:
		plugin.finish(ctx, run, StatusFailed, VerdictError, "benchmark", "不支持的测试类型")
	}
}

// configFor loads one configuration under the shared lock.
func (plugin *Plugin) configFor(ctx context.Context, id int64) (*Config, error) {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		return nil, errNotFound
	}
	return plugin.getConfig(ctx, id)
}

// credential decrypts the stored Key. A missing master key or a payload that
// fails authentication stops the task before any chargeable request.
func (plugin *Plugin) credential(config *Config) (string, error) {
	if !config.KeySet {
		return "", errors.New("该配置还没有填写 API Key")
	}
	box := plugin.cipher.Load()
	return box.open(config.keyCipher, config.keyID)
}

// runCandy submits the puzzle to the remote tester and polls for its verdict.
// The tester calls the gateway itself, so the stored Key leaves the plugin.
func (plugin *Plugin) runCandy(ctx context.Context, run *Run, config *Config, key string) {
	run.GenerationTries++
	run.Phase = PhaseTesting
	if err := plugin.persist(ctx, run); err != nil {
		return
	}
	remote := plugin.remote.Load()
	created, err := remote.create(ctx, createRequest{
		Benchmark:       BenchmarkCandy,
		BaseURL:         config.BaseURL,
		APIKey:          key,
		Model:           config.Model,
		Protocol:        "responses",
		ReasoningEffort: config.ReasoningEffort,
		ServiceTier:     config.ServiceTier,
	}, run.ID)
	if err != nil {
		plugin.failRun(ctx, run, err, key)
		return
	}
	run.remoteID = created.ID
	if err := plugin.persist(ctx, run); err != nil {
		return
	}
	result := created
	if !isTerminalStatus(created.Status) {
		result, err = remote.wait(ctx, created.ID, plugin.deadline(run))
		if err != nil {
			plugin.failRun(ctx, run, err, key)
			return
		}
	}
	plugin.applyRemote(ctx, run, result, key)
}

// runDrawing generates a document locally and submits it for classification.
// Generation and assessment retry separately with persisted attempt counters,
// so a restart cannot reset a paid budget.
func (plugin *Plugin) runDrawing(ctx context.Context, run *Run, config *Config, key string) {
	prompt := config.Prompt
	if prompt == "" {
		prompt = defaultPrompt
	}
	client := plugin.drawing.Load()
	var generated *drawingResult
	delay := 15 * time.Second
	attempt := run.GenerationTries
	for attempt < generationAttempts {
		attempt++
		run.GenerationTries = attempt
		run.Phase = PhaseGenerating
		if err := plugin.persist(ctx, run); err != nil {
			return
		}
		result, err := client.generate(ctx, config, key, prompt)
		if err == nil {
			generated = result
			break
		}
		if !isRetryable(err) || attempt >= generationAttempts || plugin.now().After(plugin.deadline(run)) {
			plugin.failRun(ctx, run, err, key)
			return
		}
		if !sleepContext(ctx, delay) {
			plugin.finish(ctx, run, StatusCancelled, VerdictUnknown, "cancelled", "插件停止，本次检测已取消")
			return
		}
		delay *= 2
	}
	if generated == nil {
		plugin.finish(ctx, run, StatusFailed, VerdictError, "generation", "HTML 生成失败，请核对使用记录")
		return
	}
	run.Question = redact(prompt, key)
	run.HTML = generated.HTML
	run.InputTokens = generated.InputTokens
	run.OutputTokens = generated.OutputTokens
	run.ReasoningTokens = generated.ReasoningTokens
	run.DurationMS = &generated.DurationMS
	run.FirstTokenMS = generated.FirstTokenMS
	run.Phase = PhaseGenerated
	if err := plugin.persist(ctx, run); err != nil {
		return
	}
	plugin.assess(ctx, run, generated.HTML, key)
}

// assess submits the saved document for classification, reusing one
// idempotency key so a repeated submission cannot create a second assessment.
func (plugin *Plugin) assess(ctx context.Context, run *Run, html, key string) {
	remote := plugin.remote.Load()
	identity := assessmentIdentity(run.ID, html)
	delay := 15 * time.Second
	for run.AssessmentTries < assessmentAttempts {
		run.AssessmentTries++
		run.Phase = PhaseAssessing
		if err := plugin.persist(ctx, run); err != nil {
			return
		}
		created, err := remote.create(ctx, createRequest{Benchmark: "html", HTML: html}, identity)
		if err != nil {
			if !isRetryable(err) || plugin.now().After(plugin.deadline(run)) {
				plugin.failAssessment(ctx, run, err)
				return
			}
			if !sleepContext(ctx, delay) {
				plugin.finish(ctx, run, StatusCancelled, VerdictUnknown, "cancelled", "插件停止，本次评价已取消")
				return
			}
			delay *= 2
			continue
		}
		result := created
		if !isTerminalStatus(created.Status) {
			result, err = remote.wait(ctx, created.ID, plugin.deadline(run))
			if err != nil {
				if !isRetryable(err) {
					plugin.failAssessment(ctx, run, err)
					return
				}
				continue
			}
		}
		plugin.applyAssessment(ctx, run, result)
		return
	}
	plugin.finish(ctx, run, StatusSucceeded, VerdictUnknown, "assessment", "评价次数已用尽，作品仍可下载")
}

// applyRemote stores a candy observation and its verdict.
func (plugin *Plugin) applyRemote(ctx context.Context, run *Run, result *remoteTest, key string) {
	verdict := result.outcome()
	if result.Candy != nil {
		run.Question = redact(result.Candy.Question, key)
		run.Answer = redact(result.Candy.Answer, key)
		run.InputTokens = result.Candy.InputTokens
		run.OutputTokens = result.Candy.OutputTokens
		run.ReasoningTokens = result.Candy.ReasoningTokens
		run.DurationMS = result.Candy.DurationMS
	}
	status := StatusSucceeded
	code, message := "", ""
	if verdict == VerdictError {
		status = StatusFailed
		code, message = remoteFailureSummary(result)
	}
	if verdict == VerdictPending {
		verdict = VerdictUnknown
		message = "检测服务未在观察期限内返回结论"
		code = "timeout"
	}
	plugin.finish(ctx, run, status, verdict, code, message)
}

// applyAssessment stores the classifier verdict for a drawing.
func (plugin *Plugin) applyAssessment(ctx context.Context, run *Run, result *remoteTest) {
	verdict := result.outcome()
	if verdict == VerdictPending {
		verdict = VerdictUnknown
	}
	if result.Assessment != nil {
		run.AssessmentReason = truncate(result.Assessment.Reason, 500)
	}
	status := StatusSucceeded
	code, message := "", ""
	if verdict == VerdictError {
		status = StatusFailed
		code, message = remoteFailureSummary(result)
	}
	plugin.finish(ctx, run, status, verdict, code, message)
}

// failAssessment ends a drawing whose document exists but whose classification
// could not complete. The artwork stays available for download.
func (plugin *Plugin) failAssessment(ctx context.Context, run *Run, err error) {
	code, message := failureFields(err)
	if message == "" {
		message = "评价失败，作品仍可下载"
	}
	plugin.finish(ctx, run, StatusFailed, VerdictUnknown, code, message)
}

// failRun ends a task with a classified failure.
func (plugin *Plugin) failRun(ctx context.Context, run *Run, err error, key string) {
	if errors.Is(err, context.Canceled) {
		plugin.finish(ctx, run, StatusCancelled, VerdictUnknown, "cancelled", "插件停止，本次检测已取消")
		return
	}
	code, message := failureFields(err)
	plugin.finish(ctx, run, StatusFailed, VerdictError, code, redact(message, key))
}

// failureFields maps an error to a stored code and safe message.
func failureFields(err error) (string, string) {
	var failure *remoteFailure
	if errors.As(err, &failure) {
		return failure.Category, failure.Error()
	}
	if err == nil {
		return "", ""
	}
	return "error", "检测失败，请稍后重试"
}

// remoteFailureSummary classifies a reported remote error for storage.
func remoteFailureSummary(result *remoteTest) (string, string) {
	message := ""
	if result != nil && result.Error != nil {
		message = string(result.Error)
	}
	category := remoteErrorCategory(message)
	if category == "" {
		category = "remote_error"
	}
	failure := &remoteFailure{Category: category}
	if status := remoteStatus(message); status != 0 {
		failure.Status = status
	}
	return category, failure.Error()
}

// finish writes a terminal state and prunes the configuration's retention.
func (plugin *Plugin) finish(ctx context.Context, run *Run, status, verdict, code, message string) {
	now := plugin.now().Unix()
	run.Status = status
	run.Verdict = verdict
	run.Phase = PhaseDone
	run.ErrorCode = code
	run.ErrorMessage = truncate(message, 500)
	run.FinishedAt = now
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		return
	}
	if err := plugin.saveRun(ctx, run); err != nil {
		plugin.logger.Error("save check failed", "run", run.ID, "error", err)
		return
	}
	if err := plugin.pruneRuns(ctx, run.ConfigID); err != nil {
		plugin.logger.Warn("prune checks failed", "config", run.ConfigID, "error", err)
	}
}

// persist stores intermediate progress for recovery and display.
func (plugin *Plugin) persist(ctx context.Context, run *Run) error {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		return errors.New("插件已停止")
	}
	return plugin.saveRun(ctx, run)
}

// deadline bounds one task's retry window from its creation time.
func (plugin *Plugin) deadline(run *Run) time.Time {
	if run.CreatedAt <= 0 {
		return plugin.now().Add(observationWindow)
	}
	return time.Unix(run.CreatedAt, 0).Add(observationWindow)
}

// assessmentIdentity derives the stable idempotency key for one document. The
// classifier deduplicates repeated submissions of the same artwork.
func assessmentIdentity(runID, html string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + html))
	return "html-gallery-" + hex.EncodeToString(sum[:])
}

// redact removes recognizable credentials from stored text.
func redact(text, key string) string {
	if text == "" {
		return ""
	}
	if key != "" {
		text = strings.ReplaceAll(text, key, "[redacted]")
	}
	return credentialPattern.ReplaceAllString(text, "[redacted]")
}

// truncate bounds stored free text.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// sleepContext waits for the duration or until the context ends. It reports
// false when the context ended first.
func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
