package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/plugintest"
	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
)

// plugin_test.go exercises plugin initialization, protected panel routes, durable
// observations, and retention through a temporary SQLite database and mock host.
// Administrator diagnostics remain inspectable while user reads follow current
// group authorization and quality histories contain definite judgments.

// fixture builds an initialized plugin over a temporary data directory with a
// deterministic group lookup and an address policy that accepts the local
// fixtures used by the tests.
type fixture struct {
	plugin  *Plugin
	harness *plugintest.Harness
	groups  []groupInfo
}

// newFixture starts a plugin with the supplied master key. An empty key leaves
// credential work disabled so those paths can be exercised too.
func newFixture(t *testing.T, masterKey string) *fixture {
	t.Helper()
	harness := plugintest.New(t)
	config, err := sdk.NewConfig([]byte(`{"credential_key":"` + masterKey + `"}`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	harness.Env.Config = config
	dataDir := t.TempDir()
	harness.Env.DataDir = dataDir
	plugin := New()
	plugin.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	plugin.groupsLookup = nil
	if err := plugin.Init(context.Background(), harness.Env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = plugin.Shutdown(ctx)
	})
	fixture := &fixture{plugin: plugin, harness: harness, groups: []groupInfo{
		{ID: 12, Name: "default", RateMultiplier: 1},
		{ID: 31, Name: "claude 专线", RateMultiplier: 1.35},
	}}
	plugin.groupsLookup = func(context.Context, int64) ([]groupInfo, error) {
		return fixture.groups, nil
	}
	plugin.baseURLPolicy = func(_ context.Context, raw string) (string, error) { return validateBaseURL(raw) }
	return fixture
}

// request issues one panel request as the supplied caller.
func (f *fixture) request(t *testing.T, method, path string, body any, caller sdk.Caller) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return f.harness.Request(f.plugin.Handler(), request, caller)
}

// adminCaller and userCaller name the two audiences used by the tests.
func adminCaller() sdk.Caller { return sdk.Caller{UserID: 1, Username: "admin", Role: "admin"} }
func userCaller() sdk.Caller  { return sdk.Caller{UserID: 9, Username: "user", Role: "user"} }

// decode reads a JSON response.
func decode(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
}

// TestInitRequiresDataDir checks that a missing or relative directory fails.
func TestInitRequiresDataDir(t *testing.T) {
	for _, value := range []string{"", "relative/path"} {
		harness := plugintest.New(t)
		harness.Env.DataDir = value
		if err := New().Init(context.Background(), harness.Env); err == nil {
			t.Fatalf("Init accepted DataDir %q", value)
		}
	}
}

// TestInitCreatesDatabase checks the expected file and its permissions.
func TestInitCreatesDatabase(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	path := filepath.Join(fixture.harness.Env.DataDir, "plugin.sqlite")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("database missing: %v", err)
	}
	// Windows does not model POSIX permission bits, so the check only means
	// something on a platform that reports them.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions = %v", info.Mode().Perm())
	}
}

// TestConfigLifecycle covers create, list, edit, and delete.
func TestConfigLifecycle(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	input := map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"protocol": "responses", "reasoning_effort": "max",
		"enabled": true, "schedule_enabled": true, "interval_minutes": 60,
	}
	created := fixture.request(t, http.MethodPost, "/admin/configs", input, adminCaller())
	if created.Code != http.StatusOK {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var createBody struct {
		Config configView `json:"config"`
	}
	decode(t, created, &createBody)
	if createBody.Config.Config.ID == 0 {
		t.Fatal("create returned no identifier")
	}
	if !createBody.Config.Config.KeySet {
		t.Fatal("the saved configuration does not report a stored Key")
	}
	if createBody.Config.Config.NextRunAt == 0 {
		t.Fatal("a scheduled configuration has no next run time")
	}
	// The response must never carry the credential or its ciphertext.
	if strings.Contains(created.Body.String(), "sk-live") {
		t.Fatal("the response leaked the plaintext Key")
	}

	list := fixture.request(t, http.MethodGet, "/admin/configs", nil, adminCaller())
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d", list.Code)
	}
	var listBody struct {
		Enabled      bool         `json:"enabled"`
		MasterKeySet bool         `json:"master_key_set"`
		Configs      []configView `json:"configs"`
	}
	decode(t, list, &listBody)
	if !listBody.MasterKeySet {
		t.Fatal("the master key is reported as missing")
	}
	if len(listBody.Configs) != 1 {
		t.Fatalf("list returned %d configurations", len(listBody.Configs))
	}

	// Editing without a Key keeps the stored credential.
	edit := map[string]any{
		"base_url": "https://relay.example.com/v1", "protocol": "responses",
		"enabled": true, "schedule_enabled": false, "interval_minutes": 120,
	}
	updated := fixture.request(t, http.MethodPut, "/admin/configs/1", edit, adminCaller())
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d %s", updated.Code, updated.Body.String())
	}
	var updateBody struct {
		Config configView `json:"config"`
	}
	decode(t, updated, &updateBody)
	if !updateBody.Config.Config.KeySet {
		t.Fatal("the edit dropped the stored Key")
	}
	if updateBody.Config.Config.ScheduleEnabled || updateBody.Config.Config.NextRunAt != 0 {
		t.Fatal("disabling the schedule left a planned run")
	}
	if updateBody.Config.Config.GroupName != "default" {
		t.Fatalf("group name = %q", updateBody.Config.Config.GroupName)
	}
	// Identity fields stay fixed even when the request supplies others.
	rejected := fixture.request(t, http.MethodPut, "/admin/configs/1", map[string]any{
		"model": "claude-fable-5-2", "base_url": "https://relay.example.com/v1",
		"enabled": true, "interval_minutes": 60,
	}, adminCaller())
	decode(t, rejected, &updateBody)
	if updateBody.Config.Config.Model != "gpt-6-astra" {
		t.Fatalf("edit changed the immutable model to %q", updateBody.Config.Config.Model)
	}

	deleted := fixture.request(t, http.MethodDelete, "/admin/configs/1", nil, adminCaller())
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete = %d", deleted.Code)
	}
	remaining := fixture.request(t, http.MethodGet, "/admin/configs", nil, adminCaller())
	decode(t, remaining, &listBody)
	if len(listBody.Configs) != 0 {
		t.Fatal("the deleted configuration is still listed")
	}
}

// TestConfigRejectsDuplicateIdentity checks the unique identity constraint.
func TestConfigRejectsDuplicateIdentity(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	input := map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "interval_minutes": 60,
	}
	if code := fixture.request(t, http.MethodPost, "/admin/configs", input, adminCaller()).Code; code != http.StatusOK {
		t.Fatalf("first create = %d", code)
	}
	duplicate := fixture.request(t, http.MethodPost, "/admin/configs", input, adminCaller())
	if duplicate.Code != http.StatusInternalServerError {
		t.Fatalf("duplicate create = %d %s", duplicate.Code, duplicate.Body.String())
	}
}

// TestConfigRequiresMasterKey checks that a Key cannot be stored without the
// master key, and that no plaintext fallback exists.
func TestConfigRequiresMasterKey(t *testing.T) {
	fixture := newFixture(t, "")
	input := map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "interval_minutes": 60,
	}
	response := fixture.request(t, http.MethodPost, "/admin/configs", input, adminCaller())
	if response.Code != http.StatusConflict {
		t.Fatalf("create without a master key = %d %s", response.Code, response.Body.String())
	}
}

// TestUserViewFiltersUnauthorizedGroups checks that a caller only sees results
// for groups the host reports as available.
func TestUserViewFiltersUnauthorizedGroups(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.groups = []groupInfo{{ID: 12, Name: "default", RateMultiplier: 1}}
	create := func(group int64, model string) {
		t.Helper()
		response := fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
			"group_id": group, "model": model, "benchmark": "candy",
			"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
			"enabled": true, "interval_minutes": 60,
		}, adminCaller())
		if response.Code != http.StatusOK {
			t.Fatalf("create = %d %s", response.Code, response.Body.String())
		}
	}
	fixture.groups = []groupInfo{{ID: 12, Name: "default"}, {ID: 31, Name: "claude 专线"}}
	create(12, "gpt-6-astra")
	create(31, "claude-fable-5-2")
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())

	// The caller now only has group 12 available.
	fixture.groups = []groupInfo{{ID: 12, Name: "default", RateMultiplier: 1}}
	response := fixture.request(t, http.MethodGet, "/tests", nil, userCaller())
	if response.Code != http.StatusOK {
		t.Fatalf("user list = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Groups []struct {
			ID      int64        `json:"id"`
			Configs []configView `json:"configs"`
		} `json:"groups"`
	}
	decode(t, response, &body)
	if len(body.Groups) != 1 || body.Groups[0].ID != 12 {
		t.Fatalf("user saw groups %+v", body.Groups)
	}
	if len(body.Groups[0].Configs) != 1 {
		t.Fatalf("user saw %d configurations", len(body.Groups[0].Configs))
	}
	if strings.Contains(response.Body.String(), "claude-fable-5-2") {
		t.Fatal("the user response leaked an unauthorized group's configuration")
	}
}

// TestUserViewRequiresFeatureSwitch checks that disabling the feature hides the
// user endpoint while administrators keep management access.
func TestUserViewRequiresFeatureSwitch(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	disabled := fixture.request(t, http.MethodGet, "/tests", nil, userCaller())
	if disabled.Code != http.StatusForbidden {
		t.Fatalf("disabled user list = %d", disabled.Code)
	}
	admin := fixture.request(t, http.MethodGet, "/admin/configs", nil, adminCaller())
	if admin.Code != http.StatusOK {
		t.Fatalf("admin list while disabled = %d", admin.Code)
	}
}

// TestAdminRoutesRejectUsers checks the audience check on every admin path.
func TestAdminRoutesRejectUsers(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/admin/configs"},
		{http.MethodPost, "/admin/configs"},
		{http.MethodPut, "/admin/settings"},
		{http.MethodGet, "/admin/settings"},
		{http.MethodPost, "/admin/actions/1/run"},
	} {
		response := fixture.request(t, route.method, route.path, map[string]any{}, userCaller())
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403", route.method, route.path, response.Code)
		}
	}
}

// TestRunNowEnqueuesOnce checks the single active task rule.
func TestRunNowEnqueuesOnce(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "interval_minutes": 60,
	}, adminCaller())
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())
	first := fixture.request(t, http.MethodPost, "/admin/actions/1/run", nil, adminCaller())
	if first.Code != http.StatusAccepted {
		t.Fatalf("first run = %d %s", first.Code, first.Body.String())
	}
	second := fixture.request(t, http.MethodPost, "/admin/actions/1/run", nil, adminCaller())
	if second.Code != http.StatusConflict {
		t.Fatalf("second run = %d %s", second.Code, second.Body.String())
	}
}

// TestRunNowRejectsMissingPrerequisites checks the guards before queueing.
func TestRunNowRejectsMissingPrerequisites(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "interval_minutes": 60,
	}, adminCaller())
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())
	// Stop the configuration, then try to run it.
	fixture.request(t, http.MethodPut, "/admin/configs/1", map[string]any{
		"base_url": "https://relay.example.com/v1", "enabled": false, "interval_minutes": 60,
	}, adminCaller())
	response := fixture.request(t, http.MethodPost, "/admin/actions/1/run", nil, adminCaller())
	if response.Code != http.StatusConflict {
		t.Fatalf("disabled run = %d %s", response.Code, response.Body.String())
	}
}

// TestTickEnqueuesDueConfigs checks the scheduler and the no-catch-up rule.
func TestTickEnqueuesDueConfigs(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "schedule_enabled": true, "interval_minutes": 60,
	}, adminCaller())
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())

	// Not due yet: the planned time is an hour out.
	if err := fixture.plugin.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := fixture.plugin.mustRuns(t, 1); len(runs) != 0 {
		t.Fatalf("tick enqueued %d tasks before they were due", len(runs))
	}

	// Move the clock past the planned time.
	fixture.plugin.now = func() time.Time { return time.Unix(1_700_000_000+3601, 0) }
	if err := fixture.plugin.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	runs := fixture.plugin.mustRuns(t, 1)
	if len(runs) != 1 {
		t.Fatalf("tick enqueued %d tasks, want 1", len(runs))
	}
	if runs[0].Status != StatusQueued {
		t.Fatalf("queued task status = %q", runs[0].Status)
	}

	// A second tick with an active task must not create a duplicate.
	if err := fixture.plugin.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := fixture.plugin.mustRuns(t, 1); len(runs) != 1 {
		t.Fatalf("a second tick created another task")
	}

	// A long outage must not produce a burst: the next run is pushed past now.
	config, err := fixture.plugin.getConfig(context.Background(), 1)
	if err != nil {
		t.Fatalf("getConfig: %v", err)
	}
	if config.NextRunAt <= fixture.plugin.now().Unix() {
		t.Fatalf("next run %d is not after now %d", config.NextRunAt, fixture.plugin.now().Unix())
	}
}

// mustRuns reads the stored observations for one configuration.
func (plugin *Plugin) mustRuns(t *testing.T, configID int64) []*Run {
	t.Helper()
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	runs, err := plugin.listRuns(context.Background(), configID, 50)
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	return runs
}

// TestTickRespectsFeatureSwitch checks that a disabled feature queues nothing.
func TestTickRespectsFeatureSwitch(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "schedule_enabled": true, "interval_minutes": 5,
	}, adminCaller())
	fixture.plugin.now = func() time.Time { return time.Unix(1_700_000_000+3600, 0) }
	if err := fixture.plugin.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if runs := fixture.plugin.mustRuns(t, 1); len(runs) != 0 {
		t.Fatal("a disabled feature queued a task")
	}
}

// TestRecoverMarksUnknownSubmission checks that an interrupted submission which
// may already have been charged becomes unknown instead of being retried.
func TestRecoverMarksUnknownSubmission(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	if _, err := fixture.plugin.db.ExecContext(context.Background(), `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, created_at, updated_at)
		VALUES ('interrupted', 1, 12, 'gpt-6-astra', 'candy', 'running', 'pending', 'generating', 1, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := fixture.plugin.db.ExecContext(context.Background(), `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, remote_id, created_at, updated_at)
		VALUES ('pollable', 2, 12, 'gpt-6-astra', 'candy', 'running', 'pending', 'testing', 'task_1', 1, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := fixture.plugin.recoverRuns(context.Background()); err != nil {
		t.Fatalf("recoverRuns: %v", err)
	}
	interrupted, err := fixture.plugin.getRun(context.Background(), "interrupted")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if interrupted.Status != StatusSucceeded || interrupted.Verdict != VerdictUnknown {
		t.Fatalf("interrupted run = %s/%s", interrupted.Status, interrupted.Verdict)
	}
	if interrupted.ErrorCode != "interrupted" {
		t.Fatalf("interrupted code = %q", interrupted.ErrorCode)
	}
	pollable, err := fixture.plugin.getRun(context.Background(), "pollable")
	if err != nil {
		t.Fatalf("getRun: %v", err)
	}
	if pollable.Status != StatusQueued {
		t.Fatalf("pollable run = %q, want queued to resume polling", pollable.Status)
	}
}

// TestRetentionKeepsJudgedHistory checks the pruning boundaries.
func TestRetentionKeepsJudgedHistory(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	ctx := context.Background()
	insert := func(id, verdict, status, html string, createdAt int64) {
		t.Helper()
		if _, err := fixture.plugin.db.ExecContext(ctx, `INSERT INTO runs
			(id, config_id, group_id, model, benchmark, status, verdict, phase, html, created_at, updated_at)
			VALUES (?,1,12,'gpt-6-astra','candy',?,?,'complete',?,?,?)`,
			id, status, verdict, html, createdAt, createdAt); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// 70 judged results, 70 diagnostics, and 12 documents.
	for index := 0; index < 70; index++ {
		insert("judged-"+pad(index), VerdictPassed, StatusSucceeded, "", int64(index))
		insert("diag-"+pad(index), VerdictError, StatusFailed, "", int64(index))
	}
	for index := 0; index < 12; index++ {
		insert("doc-"+pad(index), VerdictPassed, StatusSucceeded, "<html></html>", int64(1000+index))
	}
	if err := fixture.plugin.pruneRuns(ctx, 1); err != nil {
		t.Fatalf("pruneRuns: %v", err)
	}
	var judged, diagnostics, documents int
	if err := fixture.plugin.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE verdict IN ('passed','failed')`).Scan(&judged); err != nil {
		t.Fatalf("count judged: %v", err)
	}
	if err := fixture.plugin.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE verdict IN ('unknown','error')`).Scan(&diagnostics); err != nil {
		t.Fatalf("count diagnostics: %v", err)
	}
	if err := fixture.plugin.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE html<>''`).Scan(&documents); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if judged != 60 {
		t.Fatalf("judged rows = %d, want 60", judged)
	}
	if diagnostics != 60 {
		t.Fatalf("diagnostic rows = %d, want 60", diagnostics)
	}
	if documents != 10 {
		t.Fatalf("documents = %d, want 10", documents)
	}
}

// TestDeleteRejectsActiveTask checks that an occupied configuration cannot be
// removed while a task is running.
func TestDeleteRejectsActiveTask(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
		"group_id": 12, "model": "gpt-6-astra", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "interval_minutes": 60,
	}, adminCaller())
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())
	if queued := fixture.request(t, http.MethodPost, "/admin/actions/1/run", nil, adminCaller()); queued.Code != http.StatusAccepted {
		t.Fatalf("queue = %d %s", queued.Code, queued.Body.String())
	}
	response := fixture.request(t, http.MethodDelete, "/admin/configs/1", nil, adminCaller())
	if response.Code != http.StatusConflict {
		t.Fatalf("delete with an active task = %d %s", response.Code, response.Body.String())
	}
}

// TestRunDetailAuthorization checks that a user cannot read another group's
// observation while an administrator can.
func TestRunDetailAuthorization(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	ctx := context.Background()
	if _, err := fixture.plugin.db.ExecContext(ctx, `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, created_at, updated_at)
		VALUES ('run-1', 1, 31, 'claude-fable-5-2', 'candy', 'succeeded', 'passed', 'complete', 1, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())
	fixture.groups = []groupInfo{{ID: 12, Name: "default"}}
	denied := fixture.request(t, http.MethodGet, "/runs/run-1", nil, userCaller())
	if denied.Code != http.StatusNotFound {
		t.Fatalf("cross-group read = %d %s", denied.Code, denied.Body.String())
	}
	allowed := fixture.request(t, http.MethodGet, "/admin/runs/run-1", nil, adminCaller())
	if allowed.Code != http.StatusOK {
		t.Fatalf("admin read = %d", allowed.Code)
	}
	fixture.groups = []groupInfo{{ID: 31, Name: "claude 专线"}}
	granted := fixture.request(t, http.MethodGet, "/runs/run-1", nil, userCaller())
	if granted.Code != http.StatusOK {
		t.Fatalf("authorized read = %d", granted.Code)
	}
}

// TestAdminHistoryIncludesDiagnostics reproduces a failed request alongside a
// judged result. Administrator history must expose both newest first, omit private
// payloads, reject user callers, and leave the quality strip restricted to judgments.
func TestAdminHistoryIncludesDiagnostics(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	ctx := context.Background()
	config := &Config{GroupID: 12, GroupName: "default", Model: "model-a", Benchmark: BenchmarkDrawing,
		BaseURL: "https://relay.example.com/v1", Protocol: "responses", IntervalMinutes: 60}
	id, err := fixture.plugin.insertConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	config.ID = id
	if _, err := fixture.plugin.db.ExecContext(ctx, `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, html, remote_id, error_message, created_at, updated_at)
		VALUES
		('judged', ?, 12, 'model-a', 'drawing', 'succeeded', 'passed', 'complete', '<html>private-artifact</html>', 'private-remote', '', 1, 1),
		('error', ?, 12, 'model-a', 'drawing', 'failed', 'error', 'complete', '', '', 'subscription expired', 2, 2)`, config.ID, config.ID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/configs/%d/runs", config.ID)
	response := fixture.request(t, http.MethodGet, path, nil, adminCaller())
	if response.Code != http.StatusOK {
		t.Fatalf("history = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Runs []*Run `json:"runs"` // newest observation summaries
	}
	decode(t, response, &body)
	if len(body.Runs) != 2 || body.Runs[0].ID != "error" || body.Runs[1].ID != "judged" {
		t.Fatalf("history omitted or reordered diagnostic observations: %+v", body.Runs)
	}
	if !body.Runs[1].HasHTML || strings.Contains(response.Body.String(), "private-artifact") || strings.Contains(response.Body.String(), "private-remote") {
		t.Fatal("history must advertise retained artwork while keeping payloads in authorized detail reads")
	}
	if denied := fixture.request(t, http.MethodGet, path, nil, userCaller()); denied.Code != http.StatusForbidden {
		t.Fatalf("user history access = %d", denied.Code)
	}
	if missing := fixture.request(t, http.MethodGet, "/admin/configs/999/runs", nil, adminCaller()); missing.Code != http.StatusNotFound {
		t.Fatalf("missing configuration history = %d", missing.Code)
	}
	view, err := fixture.plugin.buildConfigView(ctx, config)
	if err != nil || len(view.History) != 1 || view.History[0].ID != "judged" {
		t.Fatalf("quality history included a diagnostic: %+v, %v", view.History, err)
	}
}

// TestRunHTMLDownload checks the document endpoint and its guards.
func TestRunHTMLDownload(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	ctx := context.Background()
	if _, err := fixture.plugin.db.ExecContext(ctx, `INSERT INTO runs
		(id, config_id, group_id, model, benchmark, status, verdict, phase, html, created_at, updated_at)
		VALUES ('run-1', 1, 12, 'gpt-6-astra', 'drawing', 'succeeded', 'passed', 'complete', '<!doctype html><html><body>ok</body></html>', 1, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	response := fixture.request(t, http.MethodGet, "/admin/runs/run-1/html", nil, adminCaller())
	if response.Code != http.StatusOK {
		t.Fatalf("download = %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/octet-stream" {
		t.Fatalf("content type = %q", contentType)
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, "attachment") {
		t.Fatalf("disposition = %q", disposition)
	}
	if response.Body.String() != "<!doctype html><html><body>ok</body></html>" {
		t.Fatalf("body = %q", response.Body.String())
	}
	if _, err := fixture.plugin.db.ExecContext(ctx, `UPDATE runs SET html='' WHERE id='run-1'`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	missing := fixture.request(t, http.MethodGet, "/admin/runs/run-1/html", nil, adminCaller())
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expired document = %d", missing.Code)
	}
}

// TestSettingsRoundTrip checks that the feature switch persists across a
// restart of the same data directory.
func TestSettingsRoundTrip(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.request(t, http.MethodPut, "/admin/settings", map[string]any{"enabled": true}, adminCaller())
	if !fixture.plugin.featureEnabled(context.Background()) {
		t.Fatal("the switch was not applied in memory")
	}
	restarted := New()
	restarted.now = fixture.plugin.now
	restarted.groupsLookup = fixture.plugin.groupsLookup
	restarted.baseURLPolicy = func(_ context.Context, raw string) (string, error) { return validateBaseURL(raw) }
	if err := restarted.Init(context.Background(), fixture.harness.Env); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = restarted.Shutdown(ctx)
	}()
	if !restarted.featureEnabled(context.Background()) {
		t.Fatal("the switch did not survive a restart")
	}
}

// pad formats a stable identifier suffix.
func pad(value int) string {
	return strings.Repeat("0", 3-len(itoa(value))) + itoa(value)
}

// itoa formats a small nonnegative integer.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// ensure net stays imported for the resolver fixture used by other tests.
var _ = net.IPv4len

// TestConfigViewCarriesGroupRate checks that the card header can show the
// group's billing multiplier from the same response.
func TestConfigViewCarriesGroupRate(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.groups = []groupInfo{{ID: 31, Name: "claude 专线", RateMultiplier: 1.35}}
	if code := fixture.request(t, http.MethodPost, "/admin/configs", map[string]any{
		"group_id": 31, "model": "claude-fable-5-2", "benchmark": "candy",
		"base_url": "https://relay.example.com/v1", "api_key": "sk-live-abcdefghijklmnop",
		"enabled": true, "interval_minutes": 60,
	}, adminCaller()).Code; code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	response := fixture.request(t, http.MethodGet, "/admin/configs", nil, adminCaller())
	var body struct {
		Configs []configView `json:"configs"`
	}
	decode(t, response, &body)
	if len(body.Configs) != 1 {
		t.Fatalf("list returned %d configurations", len(body.Configs))
	}
	if body.Configs[0].RateMultiplier != 1.35 {
		t.Fatalf("rate multiplier = %v, want 1.35", body.Configs[0].RateMultiplier)
	}
}

// TestHistoryIsCappedAtSixty checks the strip's fixed slot count.
func TestHistoryIsCappedAtSixty(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	ctx := context.Background()
	for index := 0; index < 75; index++ {
		if _, err := fixture.plugin.db.ExecContext(ctx, `INSERT INTO runs
			(id, config_id, group_id, model, benchmark, status, verdict, phase, created_at, updated_at)
			VALUES (?,1,12,'gpt-6-astra','candy','succeeded','passed','complete',?,?)`,
			"run-"+pad(index), index+1, index+1); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	config, err := fixture.plugin.getConfig(ctx, 1)
	if err != nil {
		// The configuration row does not exist; create it so the view can be built.
		if _, err := fixture.plugin.db.ExecContext(ctx, `INSERT INTO configs
			(id, group_id, group_name, model, benchmark, base_url, created_at, updated_at)
			VALUES (1, 12, 'default', 'gpt-6-astra', 'candy', 'https://relay.example.com/v1', 1, 1)`); err != nil {
			t.Fatalf("insert config: %v", err)
		}
		if config, err = fixture.plugin.getConfig(ctx, 1); err != nil {
			t.Fatalf("getConfig: %v", err)
		}
	}
	view, err := fixture.plugin.buildConfigView(ctx, config)
	if err != nil {
		t.Fatalf("buildConfigView: %v", err)
	}
	if len(view.History) != 60 {
		t.Fatalf("history holds %d runs, want 60", len(view.History))
	}
	// Oldest first: the first slot is the 15th run once 75 exist.
	if view.History[0].CreatedAt != 16 {
		t.Fatalf("first history entry created at %d, want 16", view.History[0].CreatedAt)
	}
	if view.Judged != 75 {
		t.Fatalf("judged count = %d, want 75", view.Judged)
	}
	if view.PassRate == nil || *view.PassRate != 100 {
		t.Fatalf("pass rate = %v", view.PassRate)
	}
}

// TestAdminGroupsListsMultipliersAndModels checks the catalogue that fills the
// configuration form: group identity, billing multiplier, and model options.
func TestAdminGroupsListsMultipliersAndModels(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.groups = []groupInfo{
		{ID: 12, Name: "default", RateMultiplier: 1},
		{ID: 31, Name: "claude 专线", RateMultiplier: 1.35},
	}
	fixture.plugin.modelsLookup = func(context.Context) ([]byte, error) {
		return []byte(`{"code":0,"data":[
			{"id":12,"name":"default","models":[
				{"id":"gpt-6-astra","display_name":"GPT-6 Astra"},
				{"id":"claude-fable-5-2","display_name":""}
			]},
			{"id":99,"name":"private","models":[{"id":"secret-model","display_name":"Secret"}]}
		]}`), nil
	}
	response := fixture.request(t, http.MethodGet, "/admin/groups", nil, adminCaller())
	if response.Code != http.StatusOK {
		t.Fatalf("groups = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Groups []catalogGroup `json:"groups"`
	}
	decode(t, response, &body)
	if len(body.Groups) != 2 {
		t.Fatalf("returned %d groups, want the caller's two", len(body.Groups))
	}
	first := body.Groups[0]
	if first.ID != 12 || first.Name != "default" || first.RateMultiplier != 1 {
		t.Fatalf("first group = %+v", first)
	}
	if len(first.Models) != 2 {
		t.Fatalf("first group lists %d models", len(first.Models))
	}
	// A missing display name falls back to the identifier.
	if first.Models[1].Name != "claude-fable-5-2" {
		t.Fatalf("fallback name = %q", first.Models[1].Name)
	}
	if body.Groups[1].RateMultiplier != 1.35 {
		t.Fatalf("second multiplier = %v", body.Groups[1].RateMultiplier)
	}
	// A group outside the caller's authorization never appears, even when the
	// marketplace lists it.
	for _, group := range body.Groups {
		if group.ID == 99 {
			t.Fatal("an unauthorized group leaked into the catalogue")
		}
	}
	// The second group has no marketplace entry, so its model list stays empty
	// rather than absent.
	if body.Groups[1].Models == nil || len(body.Groups[1].Models) != 0 {
		t.Fatalf("second group models = %v, want an empty list", body.Groups[1].Models)
	}
}

// TestAdminGroupsSurvivesMarketplaceFailure checks that a catalogue read failure
// still returns the groups so manual model entry stays available.
func TestAdminGroupsSurvivesMarketplaceFailure(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	fixture.groups = []groupInfo{{ID: 12, Name: "default", RateMultiplier: 1}}
	fixture.plugin.modelsLookup = func(context.Context) ([]byte, error) {
		return nil, errors.New("marketplace unavailable")
	}
	response := fixture.request(t, http.MethodGet, "/admin/groups", nil, adminCaller())
	if response.Code != http.StatusOK {
		t.Fatalf("groups = %d", response.Code)
	}
	var body struct {
		Groups []catalogGroup `json:"groups"`
	}
	decode(t, response, &body)
	if len(body.Groups) != 1 || len(body.Groups[0].Models) != 0 {
		t.Fatalf("groups = %+v", body.Groups)
	}
}

// TestAdminGroupsRejectsUsers checks the audience check on the catalogue route.
func TestAdminGroupsRejectsUsers(t *testing.T) {
	fixture := newFixture(t, strings.Repeat("k", masterKeyMinLength))
	response := fixture.request(t, http.MethodGet, "/admin/groups", nil, userCaller())
	if response.Code != http.StatusForbidden {
		t.Fatalf("user catalogue read = %d", response.Code)
	}
}
