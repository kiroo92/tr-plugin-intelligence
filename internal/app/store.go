package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// store.go owns the plugin's SQLite schema and every query against it.
// Configurations keep an immutable identity, runs keep the full observation,
// and a partial unique index enforces one active task per configuration.

// schema statements create the tables and indexes on every start. They are
// idempotent so an existing database is upgraded in place.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS configs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		group_id INTEGER NOT NULL,
		group_name TEXT NOT NULL,
		model TEXT NOT NULL,
		benchmark TEXT NOT NULL,
		prompt TEXT NOT NULL DEFAULT '',
		base_url TEXT NOT NULL,
		protocol TEXT NOT NULL DEFAULT 'responses',
		reasoning_effort TEXT NOT NULL DEFAULT '',
		service_tier TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		schedule_enabled INTEGER NOT NULL DEFAULT 0,
		interval_minutes INTEGER NOT NULL DEFAULT 60,
		next_run_at INTEGER NOT NULL DEFAULT 0,
		key_id TEXT NOT NULL DEFAULT '',
		key_cipher TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE (group_id, model, benchmark)
	)`,
	`CREATE TABLE IF NOT EXISTS runs (
		id TEXT PRIMARY KEY,
		config_id INTEGER NOT NULL,
		group_id INTEGER NOT NULL,
		model TEXT NOT NULL,
		benchmark TEXT NOT NULL,
		status TEXT NOT NULL,
		verdict TEXT NOT NULL,
		phase TEXT NOT NULL,
		question TEXT NOT NULL DEFAULT '',
		answer TEXT NOT NULL DEFAULT '',
		html TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER,
		output_tokens INTEGER,
		reasoning_tokens INTEGER,
		duration_ms INTEGER,
		first_token_ms INTEGER,
		assessment_reason TEXT NOT NULL DEFAULT '',
		error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		remote_id TEXT NOT NULL DEFAULT '',
		generation_tries INTEGER NOT NULL DEFAULT 0,
		assessment_tries INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		finished_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS runs_by_config ON runs (config_id, created_at DESC)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active ON runs (config_id)
		WHERE status IN ('queued', 'running')`,
	`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

// migrate applies the schema. Every statement is idempotent, so a restart or an
// upgrade over an existing database only adds what is missing.
func migrate(ctx context.Context, db *sql.DB) error {
	for _, statement := range schema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("无法初始化插件数据库: %w", err)
		}
	}
	return nil
}

// pluginConfig is the row scanner shared by configuration queries.
const configColumns = `id, group_id, group_name, model, benchmark, prompt, base_url,
	protocol, reasoning_effort, service_tier, enabled, schedule_enabled, interval_minutes,
	next_run_at, key_id, key_cipher, created_at, updated_at`

// scanConfig reads one configuration row.
func scanConfig(scanner interface{ Scan(...any) error }) (*Config, error) {
	var config Config
	var enabled, schedule int
	err := scanner.Scan(
		&config.ID, &config.GroupID, &config.GroupName, &config.Model, &config.Benchmark,
		&config.Prompt, &config.BaseURL, &config.Protocol, &config.ReasoningEffort,
		&config.ServiceTier, &enabled, &schedule, &config.IntervalMinutes, &config.NextRunAt,
		&config.keyID, &config.keyCipher, &config.CreatedAt, &config.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	config.Enabled = enabled != 0
	config.ScheduleEnabled = schedule != 0
	config.KeySet = config.keyCipher != ""
	return &config, nil
}

// listConfigs returns every configuration ordered by group then identity.
func (plugin *Plugin) listConfigs(ctx context.Context) ([]*Config, error) {
	rows, err := plugin.db.QueryContext(ctx, `SELECT `+configColumns+` FROM configs ORDER BY group_id, model, benchmark`)
	if err != nil {
		return nil, errors.New("读取检测配置失败")
	}
	defer rows.Close()
	var configs []*Config
	for rows.Next() {
		config, err := scanConfig(rows)
		if err != nil {
			return nil, errors.New("读取检测配置失败")
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

// getConfig returns one configuration or errNotFound.
func (plugin *Plugin) getConfig(ctx context.Context, id int64) (*Config, error) {
	config, err := scanConfig(plugin.db.QueryRowContext(ctx, `SELECT `+configColumns+` FROM configs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, errors.New("读取检测配置失败")
	}
	return config, nil
}

// findConfig returns the configuration with the same immutable identity so a
// duplicate create can be reported before insertion.
func (plugin *Plugin) findConfig(ctx context.Context, groupID int64, model, benchmark string) (*Config, error) {
	config, err := scanConfig(plugin.db.QueryRowContext(ctx,
		`SELECT `+configColumns+` FROM configs WHERE group_id=? AND model=? AND benchmark=?`,
		groupID, model, benchmark))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, errors.New("读取检测配置失败")
	}
	return config, nil
}

// countConfigs bounds how many configurations one site may keep.
func (plugin *Plugin) countConfigs(ctx context.Context) (int, error) {
	var total int
	if err := plugin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM configs`).Scan(&total); err != nil {
		return 0, errors.New("读取检测配置失败")
	}
	return total, nil
}

// insertConfig stores a new configuration and returns its identifier.
func (plugin *Plugin) insertConfig(ctx context.Context, config *Config) (int64, error) {
	now := plugin.now().Unix()
	result, err := plugin.db.ExecContext(ctx, `INSERT INTO configs
		(group_id, group_name, model, benchmark, prompt, base_url, protocol, reasoning_effort,
		 service_tier, enabled, schedule_enabled, interval_minutes, next_run_at, key_id, key_cipher,
		 created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		config.GroupID, config.GroupName, config.Model, config.Benchmark, config.Prompt,
		config.BaseURL, config.Protocol, config.ReasoningEffort, config.ServiceTier,
		boolInt(config.Enabled), boolInt(config.ScheduleEnabled), config.IntervalMinutes,
		config.NextRunAt, config.keyID, config.keyCipher, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, errors.New("同一分组、模型和测试类型的配置已存在")
		}
		return 0, errors.New("保存检测配置失败")
	}
	return result.LastInsertId()
}

// updateConfig writes the editable fields of an existing configuration. The
// immutable identity columns are not part of this statement.
func (plugin *Plugin) updateConfig(ctx context.Context, config *Config) error {
	_, err := plugin.db.ExecContext(ctx, `UPDATE configs SET
		group_name=?, prompt=?, base_url=?, protocol=?, reasoning_effort=?, service_tier=?,
		enabled=?, schedule_enabled=?, interval_minutes=?, next_run_at=?, key_id=?, key_cipher=?,
		updated_at=? WHERE id=?`,
		config.GroupName, config.Prompt, config.BaseURL, config.Protocol, config.ReasoningEffort,
		config.ServiceTier, boolInt(config.Enabled), boolInt(config.ScheduleEnabled),
		config.IntervalMinutes, config.NextRunAt, config.keyID, config.keyCipher,
		plugin.now().Unix(), config.ID)
	if err != nil {
		return errors.New("保存检测配置失败")
	}
	return nil
}

// deleteConfig removes a configuration only when it has no active task. Its
// observations go with it so history cannot outlive the identity.
func (plugin *Plugin) deleteConfig(ctx context.Context, id int64) error {
	tx, err := plugin.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("删除检测配置失败")
	}
	defer func() { _ = tx.Rollback() }()
	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE config_id=? AND status IN ('queued','running')`, id).Scan(&active); err != nil {
		return errors.New("删除检测配置失败")
	}
	if active > 0 {
		return errBusy
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE config_id=?`, id); err != nil {
		return errors.New("删除检测配置失败")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM configs WHERE id=?`, id); err != nil {
		return errors.New("删除检测配置失败")
	}
	if err := tx.Commit(); err != nil {
		return errors.New("删除检测配置失败")
	}
	return nil
}

// dueConfigs returns configurations whose schedule is due. The caller advances
// their next run time in the same transaction that enqueues the task.
func (plugin *Plugin) dueConfigs(ctx context.Context, now int64) ([]*Config, error) {
	rows, err := plugin.db.QueryContext(ctx, `SELECT `+configColumns+` FROM configs
		WHERE enabled=1 AND schedule_enabled=1 AND next_run_at>0 AND next_run_at<=?`, now)
	if err != nil {
		return nil, errors.New("读取待检测配置失败")
	}
	defer rows.Close()
	var configs []*Config
	for rows.Next() {
		config, err := scanConfig(rows)
		if err != nil {
			return nil, errors.New("读取待检测配置失败")
		}
		configs = append(configs, config)
	}
	return configs, rows.Err()
}

// runColumns is the full observation projection used by detail reads.
const runColumns = `id, config_id, group_id, model, benchmark, status, verdict, phase,
	question, answer, html, input_tokens, output_tokens, reasoning_tokens, duration_ms,
	first_token_ms, assessment_reason, error_code, error_message, remote_id,
	generation_tries, assessment_tries, created_at, updated_at, finished_at`

// scanRun reads one observation row.
func scanRun(scanner interface{ Scan(...any) error }) (*Run, error) {
	var run Run
	err := scanner.Scan(
		&run.ID, &run.ConfigID, &run.GroupID, &run.Model, &run.Benchmark, &run.Status,
		&run.Verdict, &run.Phase, &run.Question, &run.Answer, &run.HTML,
		&run.InputTokens, &run.OutputTokens, &run.ReasoningTokens, &run.DurationMS,
		&run.FirstTokenMS, &run.AssessmentReason, &run.ErrorCode, &run.ErrorMessage,
		&run.remoteID, &run.GenerationTries, &run.AssessmentTries,
		&run.CreatedAt, &run.UpdatedAt, &run.FinishedAt,
	)
	if err != nil {
		return nil, err
	}
	run.HasHTML = run.HTML != ""
	run.hasRemoteIdentity = run.remoteID != ""
	return &run, nil
}

// newRunID produces a sortable, URL-safe observation identifier.
func newRunID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("无法生成检测编号")
	}
	return hex.EncodeToString(raw), nil
}

// enqueueRun inserts a queued task for the configuration. The partial unique
// index rejects a second active task, which also protects concurrent clicks.
func (plugin *Plugin) enqueueRun(ctx context.Context, config *Config) (*Run, error) {
	id, err := newRunID()
	if err != nil {
		return nil, err
	}
	now := plugin.now().Unix()
	_, err = plugin.db.ExecContext(ctx, `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, config.ID, config.GroupID, config.Model, config.Benchmark,
		StatusQueued, VerdictPending, PhaseQueued, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errBusy
		}
		return nil, errors.New("创建检测任务失败")
	}
	return plugin.getRun(ctx, id)
}

// enqueueScheduled inserts a queued task and advances the schedule in one
// transaction. A task already active for the configuration skips this round
// without disturbing the stored history.
func (plugin *Plugin) enqueueScheduled(ctx context.Context, config *Config, next int64) (bool, error) {
	tx, err := plugin.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errors.New("创建检测任务失败")
	}
	defer func() { _ = tx.Rollback() }()
	id, err := newRunID()
	if err != nil {
		return false, err
	}
	now := plugin.now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, config.ID, config.GroupID, config.Model, config.Benchmark,
		StatusQueued, VerdictPending, PhaseQueued, now, now)
	if err != nil && !strings.Contains(err.Error(), "UNIQUE") {
		return false, errors.New("创建检测任务失败")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE configs SET next_run_at=?, updated_at=? WHERE id=?`,
		next, now, config.ID); err != nil {
		return false, errors.New("推进检测计划失败")
	}
	if err := tx.Commit(); err != nil {
		return false, errors.New("创建检测任务失败")
	}
	return err == nil, nil
}

// claimRun takes the oldest queued task and marks it running. The compare in
// the UPDATE keeps two workers from claiming the same row.
func (plugin *Plugin) claimRun(ctx context.Context) (*Run, error) {
	tx, err := plugin.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("领取检测任务失败")
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE status='queued' ORDER BY created_at LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("领取检测任务失败")
	}
	now := plugin.now().Unix()
	result, err := tx.ExecContext(ctx,
		`UPDATE runs SET status='running', phase='generating', updated_at=? WHERE id=? AND status='queued'`, now, id)
	if err != nil {
		return nil, errors.New("领取检测任务失败")
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return nil, nil
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if err != nil {
		return nil, errors.New("领取检测任务失败")
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("领取检测任务失败")
	}
	return run, nil
}

// getRun returns one observation with its full payload.
func (plugin *Plugin) getRun(ctx context.Context, id string) (*Run, error) {
	run, err := scanRun(plugin.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, errors.New("读取检测记录失败")
	}
	return run, nil
}

// listRuns returns the newest observations for one configuration, newest first.
// Payload columns are read because the caller trims them into summaries.
func (plugin *Plugin) listRuns(ctx context.Context, configID int64, limit int) ([]*Run, error) {
	rows, err := plugin.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE config_id=? ORDER BY created_at DESC LIMIT ?`, configID, limit)
	if err != nil {
		return nil, errors.New("读取检测记录失败")
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, errors.New("读取检测记录失败")
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// saveRun persists the mutable observation fields for one task.
func (plugin *Plugin) saveRun(ctx context.Context, run *Run) error {
	run.HasHTML = run.HTML != ""
	_, err := plugin.db.ExecContext(ctx, `UPDATE runs SET
		status=?, verdict=?, phase=?, question=?, answer=?, html=?, input_tokens=?, output_tokens=?,
		reasoning_tokens=?, duration_ms=?, first_token_ms=?, assessment_reason=?, error_code=?,
		error_message=?, remote_id=?, generation_tries=?, assessment_tries=?, updated_at=?, finished_at=?
		WHERE id=?`,
		run.Status, run.Verdict, run.Phase, run.Question, run.Answer, run.HTML,
		run.InputTokens, run.OutputTokens, run.ReasoningTokens, run.DurationMS,
		run.FirstTokenMS, run.AssessmentReason, run.ErrorCode, run.ErrorMessage,
		run.remoteID, run.GenerationTries, run.AssessmentTries, plugin.now().Unix(),
		run.FinishedAt, run.ID)
	if err != nil {
		return errors.New("保存检测记录失败")
	}
	return nil
}

// pruneRuns enforces the retention rules for one configuration: recent judged
// observations, recent terminal diagnostics, and the newest documents. Failed
// diagnostic rows never displace a judged result.
func (plugin *Plugin) pruneRuns(ctx context.Context, configID int64) error {
	if _, err := plugin.db.ExecContext(ctx, `
		DELETE FROM runs WHERE config_id=? AND status IN ('succeeded','failed','cancelled')
		AND verdict IN ('unknown','error')
		AND id NOT IN (
			SELECT id FROM runs WHERE config_id=? AND status IN ('succeeded','failed','cancelled')
			AND verdict IN ('unknown','error') ORDER BY created_at DESC LIMIT 60
		)`, configID, configID); err != nil {
		return errors.New("清理检测记录失败")
	}
	if _, err := plugin.db.ExecContext(ctx, `
		DELETE FROM runs WHERE config_id=? AND verdict IN ('passed','failed')
		AND id NOT IN (
			SELECT id FROM runs WHERE config_id=? AND verdict IN ('passed','failed')
			ORDER BY created_at DESC LIMIT 60
		)`, configID, configID); err != nil {
		return errors.New("清理检测记录失败")
	}
	// Only retained documents are cleared; the judged summary rows stay.
	if _, err := plugin.db.ExecContext(ctx, `
		UPDATE runs SET html='' WHERE config_id=? AND html<>''
		AND id NOT IN (
			SELECT id FROM runs WHERE config_id=? AND html<>'' ORDER BY created_at DESC LIMIT 10
		)`, configID, configID); err != nil {
		return errors.New("清理检测作品失败")
	}
	return nil
}

// recoverRuns settles tasks left behind by a restart. A submission without a
// remote identifier and without a saved document may already have been charged,
// so it becomes unknown for manual reconciliation instead of being replayed.
func (plugin *Plugin) recoverRuns(ctx context.Context) error {
	if _, err := plugin.db.ExecContext(ctx, `
		UPDATE runs SET status='succeeded', verdict='unknown', phase='complete',
			error_code='interrupted', error_message='插件重启时本次请求尚未确认，可能已产生用量，请核对使用记录',
			finished_at=?, updated_at=?
		WHERE status='running' AND remote_id='' AND html=''`, plugin.now().Unix(), plugin.now().Unix()); err != nil {
		return errors.New("恢复检测记录失败")
	}
	// Tasks holding a remote identifier resume polling; queued tasks stay queued.
	if _, err := plugin.db.ExecContext(ctx, `
		UPDATE runs SET status='queued', phase='queued', updated_at=? WHERE status='running'`,
		plugin.now().Unix()); err != nil {
		return errors.New("恢复检测记录失败")
	}
	return nil
}

// setting reads one plugin-level string value.
func (plugin *Plugin) setting(ctx context.Context, key string) (string, error) {
	var value string
	err := plugin.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("读取插件设置失败")
	}
	return value, nil
}

// saveSetting writes one plugin-level string value.
func (plugin *Plugin) saveSetting(ctx context.Context, key, value string) error {
	_, err := plugin.db.ExecContext(ctx,
		`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	if err != nil {
		return errors.New("保存插件设置失败")
	}
	return nil
}

// boolInt converts a boolean for SQLite storage.
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// nextRun computes the next scheduled time from the last planned instant so a
// slow worker cannot drift the interval forward.
func nextRun(from time.Time, minutes int) int64 {
	if minutes < 1 {
		minutes = 60
	}
	return from.Add(time.Duration(minutes) * time.Minute).Unix()
}
