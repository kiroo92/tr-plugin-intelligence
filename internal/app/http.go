package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
)

// http.go exposes the administrator and read-only user API. The host has
// already authenticated each route; every handler re-checks the caller and
// re-evaluates group authorization before returning any observation.

// panelRoutes lists the local mux patterns. A trailing slash matches the whole
// subtree, which is how the host's "/*" manifest routes arrive here.
var panelRoutes = []string{
	"GET /admin/configs",
	"POST /admin/configs",
	"PUT /admin/configs/",
	"DELETE /admin/configs/",
	"POST /admin/actions/",
	"GET /admin/runs/",
	"GET /admin/settings",
	"PUT /admin/settings",
	"GET /tests",
	"GET /runs/",
}

// Handler returns the router for every declared route.
func (plugin *Plugin) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range panelRoutes {
		mux.HandleFunc(route, plugin.serve)
	}
	return mux
}

// serve authorizes one request and dispatches it by path.
func (plugin *Plugin) serve(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	caller, ok := sdk.CallerFrom(request.Context())
	if !ok || caller.UserID <= 0 {
		writeProblem(writer, http.StatusUnauthorized, "请先登录")
		return
	}
	path := request.URL.Path
	admin := strings.HasPrefix(path, "/admin/")
	if admin && caller.Role != "admin" {
		writeProblem(writer, http.StatusForbidden, "需要管理员权限")
		return
	}
	if plugin.dbHandle() == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	ctx := request.Context()
	switch {
	case path == "/admin/configs" && request.Method == http.MethodGet:
		plugin.adminListConfigs(ctx, writer, caller)
	case path == "/admin/configs" && request.Method == http.MethodPost:
		plugin.adminSaveConfig(ctx, writer, request, caller, 0)
	case strings.HasPrefix(path, "/admin/configs/"):
		plugin.adminConfigItem(ctx, writer, request, caller, strings.TrimPrefix(path, "/admin/configs/"))
	case strings.HasPrefix(path, "/admin/actions/"):
		plugin.adminAction(ctx, writer, request, caller, strings.TrimPrefix(path, "/admin/actions/"))
	case strings.HasPrefix(path, "/admin/runs/"):
		plugin.runDetail(ctx, writer, caller, strings.TrimPrefix(path, "/admin/runs/"), true)
	case path == "/admin/settings" && request.Method == http.MethodGet:
		plugin.adminGetSettings(ctx, writer)
	case path == "/admin/settings" && request.Method == http.MethodPut:
		plugin.adminSaveSettings(ctx, writer, request)
	case path == "/tests" && request.Method == http.MethodGet:
		plugin.userTests(ctx, writer, caller)
	case strings.HasPrefix(path, "/runs/"):
		plugin.runDetail(ctx, writer, caller, strings.TrimPrefix(path, "/runs/"), false)
	default:
		writeProblem(writer, http.StatusNotFound, "接口不存在")
	}
}

// dbHandle returns the current database handle under the shared lock.
func (plugin *Plugin) dbHandle() *sql.DB {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	return plugin.db
}

// adminConfigItem handles edit and delete for one identifier.
func (plugin *Plugin) adminConfigItem(ctx context.Context, writer http.ResponseWriter, request *http.Request, caller sdk.Caller, remainder string) {
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) != 1 {
		writeProblem(writer, http.StatusNotFound, "接口不存在")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeProblem(writer, http.StatusBadRequest, "检测配置编号无效")
		return
	}
	switch request.Method {
	case http.MethodPut, http.MethodPost:
		plugin.adminSaveConfig(ctx, writer, request, caller, id)
	case http.MethodDelete:
		plugin.adminDeleteConfig(ctx, writer, id)
	default:
		writeProblem(writer, http.StatusNotFound, "接口不存在")
	}
}

// adminAction handles run-now for one configuration.
func (plugin *Plugin) adminAction(ctx context.Context, writer http.ResponseWriter, request *http.Request, caller sdk.Caller, remainder string) {
	if request.Method != http.MethodPost {
		writeProblem(writer, http.StatusNotFound, "接口不存在")
		return
	}
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) != 2 || parts[1] != "run" {
		writeProblem(writer, http.StatusNotFound, "接口不存在")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeProblem(writer, http.StatusBadRequest, "检测配置编号无效")
		return
	}
	_ = caller
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	config, err := plugin.getConfig(ctx, id)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	if !plugin.featureEnabled(ctx) {
		writeFailure(writer, errDisabled)
		return
	}
	if !config.Enabled {
		writeProblem(writer, http.StatusConflict, "该配置已停用")
		return
	}
	if !config.KeySet {
		writeProblem(writer, http.StatusConflict, "该配置还没有填写 API Key")
		return
	}
	if plugin.cipher.Load() == nil {
		writeFailure(writer, errNoMasterKey)
		return
	}
	if _, err := plugin.checkBaseURL(ctx, config.BaseURL); err != nil {
		writeProblem(writer, http.StatusBadRequest, err.Error())
		return
	}
	run, err := plugin.enqueueRun(ctx, config)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	sdk.WriteJSON(writer, http.StatusAccepted, map[string]any{"run": run.summary()})
}

// adminListConfigs returns configurations with their recent verdicts.
func (plugin *Plugin) adminListConfigs(ctx context.Context, writer http.ResponseWriter, caller sdk.Caller) {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	configs, err := plugin.listConfigs(ctx)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	rates := plugin.groupRates(ctx, caller.UserID)
	views := make([]configView, 0, len(configs))
	for _, config := range configs {
		view, err := plugin.buildConfigView(ctx, config)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		view.RateMultiplier = rates[config.GroupID]
		views = append(views, view)
	}
	sdk.WriteJSON(writer, http.StatusOK, map[string]any{
		"enabled":        plugin.featureEnabled(ctx),
		"master_key_set": plugin.cipher.Load() != nil,
		"configs":        views,
	})
}

// adminSaveConfig creates or updates one configuration. Identity fields stay
// fixed on edit so existing history keeps describing the same test.
func (plugin *Plugin) adminSaveConfig(ctx context.Context, writer http.ResponseWriter, request *http.Request, caller sdk.Caller, id int64) {
	var input ConfigInput
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 128<<10))
	if err := decoder.Decode(&input); err != nil {
		writeProblem(writer, http.StatusBadRequest, "请求内容无效")
		return
	}
	normalizeInput(&input)
	if id > 0 {
		input.Benchmark = strings.TrimSpace(input.Benchmark)
	}
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}

	var existing *Config
	if id > 0 {
		current, err := plugin.getConfig(ctx, id)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		existing = current
		// Identity cannot change, so the stored values win over the request.
		input.GroupID = current.GroupID
		input.Model = current.Model
		input.Benchmark = current.Benchmark
	}
	groupName, err := plugin.groupName(ctx, caller.UserID, input.GroupID, input.GroupName)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	if err := validateInput(input, groupName); err != nil {
		writeProblem(writer, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := plugin.checkBaseURL(ctx, input.BaseURL); err != nil {
		writeProblem(writer, http.StatusBadRequest, err.Error())
		return
	}
	if existing == nil {
		total, err := plugin.countConfigs(ctx)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		if total >= maxConfigs {
			writeProblem(writer, http.StatusConflict, fmt.Sprintf("检测配置最多 %d 份", maxConfigs))
			return
		}
	}

	config := &Config{
		ID:              id,
		GroupID:         input.GroupID,
		GroupName:       groupName,
		Model:           input.Model,
		Benchmark:       input.Benchmark,
		Prompt:          input.Prompt,
		BaseURL:         input.BaseURL,
		Protocol:        input.Protocol,
		ReasoningEffort: input.ReasoningEffort,
		ServiceTier:     input.ServiceTier,
		Enabled:         input.Enabled,
		ScheduleEnabled: input.ScheduleEnabled,
		IntervalMinutes: input.IntervalMinutes,
	}
	if existing != nil {
		config.keyID = existing.keyID
		config.keyCipher = existing.keyCipher
		config.KeySet = existing.KeySet
		config.NextRunAt = existing.NextRunAt
		if config.ScheduleEnabled && config.NextRunAt <= 0 {
			config.NextRunAt = plugin.now().Add(nowPlus(config.IntervalMinutes)).Unix()
		}
		if !config.ScheduleEnabled {
			config.NextRunAt = 0
		}
	} else if config.ScheduleEnabled {
		config.NextRunAt = plugin.now().Add(nowPlus(config.IntervalMinutes)).Unix()
	}

	if input.APIKey != "" {
		box := plugin.cipher.Load()
		if box == nil {
			writeFailure(writer, errNoMasterKey)
			return
		}
		keyID := config.keyID
		if keyID == "" {
			generated, err := newKeyID()
			if err != nil {
				writeFailure(writer, err)
				return
			}
			keyID = generated
		}
		ciphertext, err := box.seal(input.APIKey, keyID)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		config.keyID = keyID
		config.keyCipher = ciphertext
		config.KeySet = true
	} else if existing == nil {
		writeProblem(writer, http.StatusBadRequest, "新建配置必须填写 API Key")
		return
	}

	if existing == nil {
		newID, err := plugin.insertConfig(ctx, config)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		config.ID = newID
	} else {
		config.CreatedAt = existing.CreatedAt
		if err := plugin.updateConfig(ctx, config); err != nil {
			writeFailure(writer, err)
			return
		}
	}
	saved, err := plugin.getConfig(ctx, config.ID)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	view, err := plugin.buildConfigView(ctx, saved)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	view.RateMultiplier = plugin.groupRates(ctx, caller.UserID)[saved.GroupID]
	sdk.WriteJSON(writer, http.StatusOK, map[string]any{"config": view})
}

// adminDeleteConfig removes one configuration and its history.
func (plugin *Plugin) adminDeleteConfig(ctx context.Context, writer http.ResponseWriter, id int64) {
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	if err := plugin.deleteConfig(ctx, id); err != nil {
		writeFailure(writer, err)
		return
	}
	sdk.WriteJSON(writer, http.StatusOK, map[string]bool{"deleted": true})
}

// adminGetSettings returns the plugin-level switches.
func (plugin *Plugin) adminGetSettings(ctx context.Context, writer http.ResponseWriter) {
	sdk.WriteJSON(writer, http.StatusOK, map[string]any{
		"enabled":        plugin.featureEnabled(ctx),
		"master_key_set": plugin.cipher.Load() != nil,
	})
}

// adminSaveSettings updates the feature switch.
func (plugin *Plugin) adminSaveSettings(ctx context.Context, writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Enabled bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4<<10))
	if err := decoder.Decode(&input); err != nil {
		writeProblem(writer, http.StatusBadRequest, "请求内容无效")
		return
	}
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	if err := plugin.saveSettings(ctx, settings{Enabled: input.Enabled}); err != nil {
		writeFailure(writer, err)
		return
	}
	sdk.WriteJSON(writer, http.StatusOK, map[string]any{
		"enabled":        plugin.featureEnabled(ctx),
		"master_key_set": plugin.cipher.Load() != nil,
	})
}

// userTests returns the site checks visible to the caller's current groups.
func (plugin *Plugin) userTests(ctx context.Context, writer http.ResponseWriter, caller sdk.Caller) {
	if !plugin.featureEnabled(ctx) {
		writeFailure(writer, errDisabled)
		return
	}
	groups, err := plugin.groupsFor(ctx, caller.UserID)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	allowed := make(map[int64]groupInfo, len(groups))
	for _, group := range groups {
		allowed[group.ID] = group
	}
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	configs, err := plugin.listConfigs(ctx)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	ordered := make([]int64, 0, len(groups))
	byGroup := make(map[int64][]configView)
	for _, config := range configs {
		group, ok := allowed[config.GroupID]
		if !ok || !config.Enabled {
			continue
		}
		view, err := plugin.buildConfigView(ctx, config)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		if _, seen := byGroup[group.ID]; !seen {
			ordered = append(ordered, group.ID)
		}
		view.RateMultiplier = group.RateMultiplier
		byGroup[group.ID] = append(byGroup[group.ID], view)
	}
	type groupView struct {
		ID             int64        `json:"id"`
		Name           string       `json:"name"`
		RateMultiplier float64      `json:"rate_multiplier"`
		Configs        []configView `json:"configs"`
	}
	result := make([]groupView, 0, len(ordered))
	for _, id := range ordered {
		group := allowed[id]
		result = append(result, groupView{ID: group.ID, Name: group.Name, RateMultiplier: group.RateMultiplier, Configs: byGroup[id]})
	}
	sdk.WriteJSON(writer, http.StatusOK, map[string]any{"groups": result})
}

// runDetail returns one observation, an optional document, and enforces group
// authorization for user callers.
func (plugin *Plugin) runDetail(ctx context.Context, writer http.ResponseWriter, caller sdk.Caller, remainder string, admin bool) {
	parts := strings.Split(strings.Trim(remainder, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeProblem(writer, http.StatusNotFound, "接口不存在")
		return
	}
	id := parts[0]
	wantHTML := len(parts) == 2 && parts[1] == "html"
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		writeProblem(writer, http.StatusServiceUnavailable, "插件已停止")
		return
	}
	run, err := plugin.getRun(ctx, id)
	if err != nil {
		writeFailure(writer, err)
		return
	}
	if !admin {
		if !plugin.featureEnabled(ctx) {
			writeFailure(writer, errDisabled)
			return
		}
		groups, err := plugin.groupsFor(ctx, caller.UserID)
		if err != nil {
			writeFailure(writer, err)
			return
		}
		if !containsGroup(groups, run.GroupID) {
			writeFailure(writer, errNotFound)
			return
		}
	}
	if wantHTML {
		if run.HTML == "" {
			writeProblem(writer, http.StatusNotFound, "该次检测没有可用作品")
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="drawing-%s.html"`, run.ID))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(run.HTML))
		return
	}
	// The document travels as escaped text so the panel never renders untrusted
	// markup. The browser receives exactly the bytes that were saved.
	sdk.WriteJSON(writer, http.StatusOK, map[string]any{"run": run, "html": run.HTML})
}

// configView is the list projection shared by both audiences.
type configView struct {
	Config       *Config `json:"config"`
	Latest       *Run    `json:"latest,omitempty"`
	LatestJudged *Run    `json:"latest_judged,omitempty"`
	History      []*Run  `json:"history"`
	PassRate     *int    `json:"pass_rate,omitempty"`
	Judged       int     `json:"judged"`
	Active       bool    `json:"active"`
	// RateMultiplier mirrors the group's current billing multiplier so the card
	// header can show it without a second lookup.
	RateMultiplier float64 `json:"rate_multiplier"`
}

// historySlots is the visible length of the result strip. The page pads it with
// empty slots so a sparse history still reads as a fixed timeline.
const historySlots = 60

// buildConfigView assembles one configuration row. The history slice carries up
// to sixty judged summaries, which is exactly what the strip renders.
func (plugin *Plugin) buildConfigView(ctx context.Context, config *Config) (configView, error) {
	view := configView{Config: sanitizeConfig(config), History: []*Run{}}
	runs, err := plugin.listRuns(ctx, config.ID, 4*historySlots)
	if err != nil {
		return view, err
	}
	judged := 0
	passed := 0
	for _, run := range runs {
		// The newest observation decides the phase plate, whether or not it was
		// judged, so an active or failed task is visible beside the last result.
		if view.Latest == nil {
			view.Latest = run.summary()
		}
		if run.Status == StatusQueued || run.Status == StatusRunning {
			view.Active = true
		}
		if !run.Judged() {
			continue
		}
		judged++
		if run.Verdict == VerdictPassed {
			passed++
		}
		if view.LatestJudged == nil {
			view.LatestJudged = run.summary()
		}
		if len(view.History) < historySlots {
			view.History = append(view.History, run.summary())
		}
	}
	view.Judged = judged
	if judged > 0 {
		rate := passed * 100 / judged
		view.PassRate = &rate
	}
	// History is stored newest first; the strip renders oldest first.
	for left, right := 0, len(view.History)-1; left < right; left, right = left+1, right-1 {
		view.History[left], view.History[right] = view.History[right], view.History[left]
	}
	return view, nil
}

// sanitizeConfig returns a copy without private credential fields.
func sanitizeConfig(config *Config) *Config {
	copied := *config
	copied.keyCipher = ""
	copied.keyID = ""
	return &copied
}

// groupInfo is the subset of a host group the plugin uses.
type groupInfo struct {
	ID             int64
	Name           string
	RateMultiplier float64
}

// groupsFor reads the caller's currently available groups through the
// configured lookup, which tests replace with a fixture.
func (plugin *Plugin) groupsFor(ctx context.Context, userID int64) ([]groupInfo, error) {
	lookup := plugin.groupsLookup
	if lookup == nil {
		return nil, errors.New("宿主能力不可用")
	}
	return lookup(ctx, userID)
}

// hostGroups reads the caller's currently available groups from the host.
func (plugin *Plugin) hostGroups(ctx context.Context, userID int64) ([]groupInfo, error) {
	if plugin.env == nil || plugin.env.Host == nil {
		return nil, errors.New("宿主能力不可用")
	}
	response, err := plugin.env.Host.Catalog.GroupsForUser(ctx, userID)
	if err != nil {
		return nil, errors.New("无法读取当前账号的分组权限")
	}
	groups := make([]groupInfo, 0, len(response.GetGroups()))
	for _, group := range response.GetGroups() {
		name := group.GetDisplayName()
		if name == "" {
			name = group.GetName()
		}
		groups = append(groups, groupInfo{ID: group.GetId(), Name: name, RateMultiplier: group.GetRateMultiplier()})
	}
	return groups, nil
}

// groupName resolves the display name stored with a configuration. The host
// catalog wins; a manual label covers groups the caller cannot enumerate.
func (plugin *Plugin) groupName(ctx context.Context, userID, groupID int64, manual string) (string, error) {
	if groupID <= 0 {
		return "", errors.New("请选择有效分组")
	}
	groups, err := plugin.groupsFor(ctx, userID)
	if err == nil {
		for _, group := range groups {
			if group.ID == groupID {
				return group.Name, nil
			}
		}
	}
	manual = strings.TrimSpace(manual)
	if manual == "" {
		return "", errors.New("该分组不在当前账号的可见范围内，请填写分组名称")
	}
	return manual, nil
}

// groupRates maps each visible group to its billing multiplier. A failed lookup
// yields an empty map so the page still renders without the badge value.
func (plugin *Plugin) groupRates(ctx context.Context, userID int64) map[int64]float64 {
	rates := make(map[int64]float64)
	groups, err := plugin.groupsFor(ctx, userID)
	if err != nil {
		return rates
	}
	for _, group := range groups {
		rates[group.ID] = group.RateMultiplier
	}
	return rates
}

// containsGroup reports whether the caller can read the group.
func containsGroup(groups []groupInfo, id int64) bool {
	for _, group := range groups {
		if group.ID == id {
			return true
		}
	}
	return false
}

// nowPlus converts a configured interval into a delay.
func nowPlus(minutes int) time.Duration {
	if minutes < 1 {
		minutes = 60
	}
	return time.Duration(minutes) * time.Minute
}

// writeJSON is kept behind one helper so every response stays JSON.
func writeJSON(writer http.ResponseWriter, status int, payload any) {
	sdk.WriteJSON(writer, status, payload)
}

// writeProblem writes a plain error envelope.
func writeProblem(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"message": message})
}

// writeFailure maps a module error to a status code.
func writeFailure(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeProblem(writer, http.StatusNotFound, err.Error())
	case errors.Is(err, errBusy):
		writeProblem(writer, http.StatusConflict, err.Error())
	case errors.Is(err, errDisabled):
		writeProblem(writer, http.StatusForbidden, err.Error())
	case errors.Is(err, errNoMasterKey):
		writeProblem(writer, http.StatusConflict, err.Error())
	default:
		writeProblem(writer, http.StatusInternalServerError, err.Error())
	}
}
