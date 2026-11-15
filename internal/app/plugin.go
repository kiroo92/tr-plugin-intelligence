package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/plugin-sdk/go/sdk"
	_ "modernc.org/sqlite"
)

// plugin.go owns the process lifecycle: it validates the persistent directory,
// opens SQLite, publishes configuration-derived state, and drains background
// work on shutdown. Business calls never rely on process-local caches for
// durable data.

// Plugin implements the SDK lifecycle, HTTP, and task interfaces.
type Plugin struct {
	mu       sync.Mutex                     // guards the database handle and multi-statement writes
	db       *sql.DB                        // plugin database
	env      *sdk.Env                       // process generation environment
	logger   *slog.Logger                   // structured plugin logger
	cipher   atomic.Pointer[cipherBox]      // credential sealer derived from the master key
	remote   atomic.Pointer[remoteClient]   // tester client
	drawing  atomic.Pointer[drawingClient]  // gateway generation client
	enabled  atomic.Bool                    // cached feature switch
	now      func() time.Time               // clock, replaceable in tests
	lookupIP func(string) ([]net.IP, error) // resolver, replaceable in tests
	// groupsLookup reads the caller's visible groups. Init installs the host
	// implementation; tests substitute a deterministic fixture.
	groupsLookup func(context.Context, int64) ([]groupInfo, error)
	// modelsLookup reads the public marketplace catalogue for the group form.
	// Init installs the host implementation; tests substitute a fixture.
	modelsLookup func(context.Context) ([]byte, error)
	// baseURLPolicy validates a saved gateway URL before a chargeable request.
	// Init installs the public-address check; tests may relax it for fixtures.
	baseURLPolicy func(context.Context, string) (string, error)
	cancel        context.CancelFunc // worker cancellation
	workers       sync.WaitGroup     // worker completion counter
}

// New builds an uninitialized plugin with production defaults.
func New() *Plugin {
	plugin := &Plugin{
		logger:   slog.Default(),
		now:      time.Now,
		lookupIP: net.LookupIP,
	}
	plugin.remote.Store(newRemoteClient(remoteBaseURL))
	plugin.drawing.Store(newDrawingClient())
	return plugin
}

// Init validates the persistent directory, opens the database, and starts the
// background workers. A failure prevents this process generation from serving.
func (plugin *Plugin) Init(ctx context.Context, env *sdk.Env) error {
	if env == nil || strings.TrimSpace(env.DataDir) == "" || !filepath.IsAbs(env.DataDir) {
		return errors.New("需要支持持久化目录的 kiroo92 TokenRouter v0.7.3+：DataDir 必须为非空绝对路径")
	}
	if env.Logger != nil {
		plugin.logger = env.Logger
	}
	info, err := os.Stat(env.DataDir)
	if err != nil || !info.IsDir() {
		return errors.New("插件持久化目录不可用")
	}
	probe, err := os.CreateTemp(env.DataDir, ".write-check-")
	if err != nil {
		return errors.New("插件持久化目录不可写")
	}
	probeName := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probeName)
	if closeErr != nil || removeErr != nil {
		return errors.New("插件持久化目录写入检查失败")
	}
	databasePath := filepath.Join(env.DataDir, "plugin.sqlite")
	if existing, err := os.Lstat(databasePath); err == nil &&
		(existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular()) {
		return errors.New("数据库路径必须为普通文件")
	} else if err != nil && !os.IsNotExist(err) {
		return errors.New("无法检查数据库路径")
	}
	file, err := os.OpenFile(databasePath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("无法创建插件数据库")
	}
	permissionErr := file.Chmod(0600)
	closeFileErr := file.Close()
	if permissionErr != nil || closeFileErr != nil {
		return errors.New("无法设置数据库权限")
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return errors.New("无法打开插件数据库")
	}
	// One connection serializes writes and keeps the WAL predictable under the
	// two workers and the HTTP handlers sharing this handle.
	database.SetMaxOpenConns(1)
	fail := func(err error) error {
		_ = database.Close()
		return err
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := database.ExecContext(ctx, pragma); err != nil {
			return fail(errors.New("无法初始化插件数据库"))
		}
	}
	if err := migrate(ctx, database); err != nil {
		return fail(err)
	}
	plugin.mu.Lock()
	plugin.db = database
	plugin.env = env
	plugin.groupsLookup = plugin.hostGroups
	plugin.modelsLookup = plugin.hostModels
	plugin.baseURLPolicy = plugin.resolvePolicy
	plugin.mu.Unlock()

	if err := plugin.loadSettings(ctx); err != nil {
		return fail(err)
	}
	if err := plugin.applyConfig(env.Config); err != nil {
		return fail(err)
	}
	if err := plugin.recoverRuns(ctx); err != nil {
		plugin.logger.Warn("recover interrupted checks failed", "error", err)
	}
	background, cancel := context.WithCancel(context.Background())
	plugin.cancel = cancel
	for index := 0; index < workerCount; index++ {
		plugin.workers.Add(1)
		go plugin.worker(background)
	}
	return nil
}

// applyConfig rebuilds the credential sealer. A missing or short master key
// disables credential work but leaves stored ciphertext untouched, so filling
// the field later restores access without re-entering every Key.
func (plugin *Plugin) applyConfig(config sdk.Config) error {
	box, err := newCipherBox(config.Secret("credential_key"))
	if err != nil {
		plugin.cipher.Store(nil)
		slog.Default().Warn("credential master key unavailable")
		return nil
	}
	plugin.cipher.Store(box)
	return nil
}

// ConfigChanged applies a new configuration snapshot without a restart.
func (plugin *Plugin) ConfigChanged(_ context.Context, config sdk.Config) error {
	return plugin.applyConfig(config)
}

// loadSettings reads the persisted feature switch.
func (plugin *Plugin) loadSettings(ctx context.Context) error {
	raw, err := plugin.setting(ctx, "settings")
	if err != nil {
		return err
	}
	current := defaultSettings()
	if raw != "" {
		if json.Unmarshal([]byte(raw), &current) != nil {
			return errors.New("插件设置损坏，请恢复备份")
		}
	}
	plugin.enabled.Store(current.Enabled)
	return nil
}

// saveSettings persists the feature switch and publishes it.
func (plugin *Plugin) saveSettings(ctx context.Context, current settings) error {
	raw, err := json.Marshal(current)
	if err != nil {
		return errors.New("保存插件设置失败")
	}
	if err := plugin.saveSetting(ctx, "settings", string(raw)); err != nil {
		return err
	}
	plugin.enabled.Store(current.Enabled)
	return nil
}

// featureEnabled reports the current feature switch.
func (plugin *Plugin) featureEnabled(context.Context) bool {
	return plugin.enabled.Load()
}

// Health reports whether the database can still serve business calls.
func (plugin *Plugin) Health(ctx context.Context) error {
	plugin.mu.Lock()
	database := plugin.db
	plugin.mu.Unlock()
	if database == nil {
		return errors.New("插件数据库未就绪")
	}
	if err := database.PingContext(ctx); err != nil {
		return errors.New("插件数据库不可用")
	}
	return nil
}

// Shutdown stops the workers, waits for the current task to drain, and closes
// the database. The host has already stopped admitting requests.
func (plugin *Plugin) Shutdown(ctx context.Context) error {
	if plugin.cancel != nil {
		plugin.cancel()
		waited := make(chan struct{})
		go func() {
			plugin.workers.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-ctx.Done():
			// The drain budget expired; closing the handle below still runs so a
			// restarted process opens a clean database.
		}
	}
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if plugin.db == nil {
		return nil
	}
	err := plugin.db.Close()
	plugin.db = nil
	if err != nil {
		return fmt.Errorf("关闭插件数据库失败: %w", err)
	}
	return nil
}
