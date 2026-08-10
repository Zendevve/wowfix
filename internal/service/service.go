// Package service exposes the wowfix core (scan, fix, validate, install)
// as a Wails-bound facade. Every method returns plain JSON-marshalled
// DTOs so the frontend never sees internal model types.
//
// The package deliberately performs no prompting: confirmation flags
// arrive from the frontend as method arguments (allowDestructive /
// allowReplace). It never touches the filesystem except through the
// core packages and never blocks on user input.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wowfix/wowfix/internal/backup"
	"github.com/wowfix/wowfix/internal/catalog"
	"github.com/wowfix/wowfix/internal/config"
	"github.com/wowfix/wowfix/internal/detector"
	"github.com/wowfix/wowfix/internal/fixer"
	"github.com/wowfix/wowfix/internal/importexport"
	"github.com/wowfix/wowfix/internal/installer"
	"github.com/wowfix/wowfix/internal/logger"
	"github.com/wowfix/wowfix/internal/models"
	"github.com/wowfix/wowfix/internal/profiles"
	"github.com/wowfix/wowfix/internal/savedvars"
	"github.com/wowfix/wowfix/internal/scanner"
	"github.com/wowfix/wowfix/internal/utils"
	"github.com/wowfix/wowfix/internal/validator"
)

// Service is the Wails-bound backend facade.
type Service struct {
	store *config.Store
	log   *logger.Logger
	// Version is the build version reported in AppState; defaults to "dev".
	Version string
	// httpClient is used for catalog provider traffic; nil uses
	// http.DefaultClient. Tests point it at an in-memory mock.
	httpClient *http.Client
	// registryPath overrides the catalog registry location; empty uses
	// catalog.DefaultPath(). Tests isolate the registry in a temp dir.
	registryPath string
	// enabledProviders selects the catalog providers; nil enables all.
	// Tests enable only the provider they mock.
	enabledProviders map[string]bool
	// autoDetect finds every WoW installation on the host; nil falls
	// back to detector.AutoDetect. Tests point it at a temp fixture
	// (AutoDetect cannot be pointed at temp dirs).
	autoDetect func(context.Context) ([]detector.Installation, error)
	// wagoDirOverride pins where Wago imports are saved; empty uses the
	// user Downloads folder (or the config directory). Tests set it to
	// a temp dir.
	wagoDirOverride string
}

// New returns a Service backed by store. A nil store falls back to the
// platform user config store.
func New(store *config.Store) *Service {
	if store == nil {
		if s, err := config.NewStore(); err == nil {
			store = s
		} else {
			// Last resort: a writable temp store so the GUI still runs.
			store = config.NewStoreAt(filepath.Join(os.TempDir(), "wowfix-config.json"))
		}
	}
	return &Service{store: store, log: logger.New(500), Version: "dev", autoDetect: detector.AutoDetect}
}

// env bundles the resolved runtime context: config, detected install
// and active profile.
type env struct {
	store   *config.Store
	cfg     *config.Config
	install *detector.Installation
	profile *models.Profile
}

// resolveInstall resolves the configured installation, falling back to
// auto-detection when no path is saved. It is strict: any resolution
// failure is returned as an error for callers that require an install.
func (s *Service) resolveInstall(cfg *config.Config) (*detector.Installation, error) {
	if cfg.WoWPath != "" {
		return detector.DetectPath(cfg.WoWPath)
	}
	installs, err := detector.AutoDetect(context.Background())
	if err != nil {
		return nil, err
	}
	if len(installs) == 0 {
		return nil, nil
	}
	return &installs[0], nil
}

// env loads the config, resolves the installation (the saved wow_path,
// or auto-detection) and picks the active profile.
func (s *Service) env() (*env, error) {
	cfg, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	install, err := s.resolveInstall(cfg)
	if err != nil {
		return nil, err
	}
	return s.buildEnv(cfg, install), nil
}

// buildEnv assembles the runtime context from a loaded config and a
// possibly nil install, applying the install's profile override.
func (s *Service) buildEnv(cfg *config.Config, install *detector.Installation) *env {
	profile := models.ProfileByID(cfg.Profile)
	if profile == nil {
		profile = models.DefaultProfile()
	}
	if install != nil && install.ProfileID != "" {
		if p := models.ProfileByID(install.ProfileID); p != nil {
			profile = p
		}
	}
	return &env{store: s.store, cfg: cfg, install: install, profile: profile}
}

// requireInstall returns the resolved env or a clear error when no
// installation is available.
func (s *Service) requireInstall() (*env, error) {
	e, err := s.env()
	if err != nil {
		return nil, err
	}
	if e.install == nil {
		return nil, fmt.Errorf("no WoW installation found")
	}
	return e, nil
}

// scan runs a fresh scan of the environment's AddOns directory,
// creating it when the install has none yet.
func (s *Service) scan(e *env) (*models.ScanResult, error) {
	if _, err := detector.EnsureAddons(e.install); err != nil {
		return nil, err
	}
	return scanner.New(e.install.AddonsPath, e.profile).Scan(context.Background())
}

// backupRoot returns where snapshots live: the saved backups_dir, the
// Backups folder next to the game, or the config directory as a last
// resort.
func (s *Service) backupRoot(e *env) string {
	if e.cfg.BackupsDir != "" {
		return e.cfg.BackupsDir
	}
	if e.install != nil && e.install.Root != "" {
		return filepath.Join(e.install.Root, "Backups")
	}
	return filepath.Join(s.store.Dir(), "backups")
}

// backupRootFor returns where snapshots live for an arbitrary AddOns
// directory: the saved backups_dir, the Backups folder next to the
// install that owns the directory, or the config directory. It
// mirrors backupRoot for cross-install operations where the active
// install's root does not own the target addons dir.
func (s *Service) backupRootFor(e *env, addonsDir string) string {
	if e.cfg.BackupsDir != "" {
		return e.cfg.BackupsDir
	}
	if inst, err := detector.DetectPath(addonsDir); err == nil && inst.Root != "" {
		return filepath.Join(inst.Root, "Backups")
	}
	return filepath.Join(s.store.Dir(), "backups")
}

// profilesFor builds the collection manager for the environment:
// collections live at the config override or <config dir>/collections,
// and the manager gets
// the logger plus (when auto_backup is on) a backup manager for
// pre-switch snapshots.
func (s *Service) profilesFor(e *env) (*profiles.Manager, error) {
	dir := e.cfg.CollectionsDir
	if dir == "" {
		dir = filepath.Join(s.store.Dir(), "collections")
	}
	m, err := profiles.NewManager(dir, e.install.AddonsPath)
	if err != nil {
		return nil, err
	}
	m.Log = s.log
	if e.cfg.AutoBackup {
		m.Backups = backup.New(s.backupRoot(e), s.log)
	}
	return m, nil
}

// fixerOptions assembles a fixer.Options for the environment. The
// confirmation hook is replaced by the allowDestructive flag.
func (s *Service) fixerOptions(e *env, allowDestructive bool) fixer.Options {
	opts := fixer.Options{
		AddonsDir:        e.install.AddonsPath,
		Profile:          e.profile,
		Log:              s.log,
		Confirm:          func(format string, args ...any) bool { return allowDestructive },
		TrashFallbackDir: filepath.Join(s.store.Dir(), "trash"),
	}
	if e.cfg.AutoBackup {
		opts.Backups = backup.New(s.backupRoot(e), s.log)
	}
	return opts
}

// installerOptions assembles an installer.Options for the environment.
func (s *Service) installerOptions(e *env, allowReplace bool) installer.Options {
	opts := installer.Options{
		AddonsDir: e.install.AddonsPath,
		Profile:   e.profile,
		Log:       s.log,
		Confirm:   func(addonName string) bool { return allowReplace },
	}
	if e.cfg.AutoBackup {
		opts.Backups = backup.New(s.backupRoot(e), s.log)
	}
	return opts
}

// classifyTOC returns the compatibility verdict for an addon's primary
// TOC against the profile, or an "unknown" verdict when the addon has
// no parseable TOC.
func classifyTOC(a *models.Addon, profile *models.Profile) validator.Compatibility {
	toc := a.PrimaryTOC()
	if toc == nil && len(a.TOCs) > 0 {
		toc = a.TOCs[0]
	}
	if toc == nil {
		return validator.Compatibility{
			Status: models.CompatUnknown,
			Label:  "No TOC",
		}
	}
	return validator.ValidateTOC(toc, profile)
}

// errStrings flattens errors into their messages, skipping nils.
func errStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		if e != nil {
			out = append(out, e.Error())
		}
	}
	return out
}

// catalogFor builds a catalog wired with the environment's registry,
// backups, logger, profile and CurseForge API key. The registry lives
// at the conventional
// location: a missing file yields an empty registry, a corrupt one an
// error.
func (s *Service) catalogFor(e *env) (*catalog.Catalog, error) {
	reg, err := s.loadRegistry(e)
	if err != nil {
		return nil, err
	}
	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	cat, err := catalog.New(s.enabledProviders, client)
	if err != nil {
		return nil, err
	}
	cat.Reg = reg
	cat.Backups = backup.New(s.backupRoot(e), s.log)
	cat.Log = s.log
	cat.Profile = e.profile
	// The WOWFIX_CURSEFORGE_API_KEY environment variable takes
	// precedence; the saved config value is the fallback (the catalog
	// checks the env var itself, so this field only needs the config).
	key := os.Getenv("WOWFIX_CURSEFORGE_API_KEY")
	if key == "" {
		key = e.cfg.CurseForgeAPIKey
	}
	cat.CurseForgeAPIKey = key
	return cat, nil
}

// flavorLabel describes an update's game-family mismatch as a short
// human string, e.g. "retail addon · profile wrath". It is empty when
// the game version is unknown.
func flavorLabel(gameVersion string, profile *models.Profile) string {
	addon := strings.ToLower(strings.TrimSpace(gameVersion))
	if profile == nil || addon == "" {
		return ""
	}
	return fmt.Sprintf("%s addon · profile %s", addon, strings.ToLower(models.FamilyLabel(profile.Family)))
}

// folderExists reports whether addonsDir contains a directory named
// name, case-insensitively (an install may differ in case from the
// registry's tracked folder).
func folderExists(addonsDir, name string) bool {
	if _, err := os.Stat(filepath.Join(addonsDir, name)); err == nil {
		return true
	}
	entries, err := os.ReadDir(addonsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.EqualFold(e.Name(), name) {
			return true
		}
	}
	return false
}

// loadRegistry resolves the registry at the override seam or the
// conventional location and loads it. A missing file yields an empty
// registry; a corrupt one is an error.
func (s *Service) loadRegistry(e *env) (*catalog.Registry, error) {
	path := s.registryPath
	if path == "" {
		var err error
		if path, err = catalog.DefaultPath(); err != nil {
			return nil, err
		}
	}
	return catalog.NewRegistry(path)
}

// registryEntries loads the tracked registry entries keyed by lowercase
// folder. Integrity reporting is best-effort: an unreadable or missing
// registry yields an empty map so scans never fail over provenance.
func (s *Service) registryEntries(e *env) map[string]catalog.Entry {
	reg, err := s.loadRegistry(e)
	if err != nil {
		return nil
	}
	out := make(map[string]catalog.Entry, len(reg.Entries()))
	for _, ent := range reg.Entries() {
		out[strings.ToLower(ent.Folder)] = ent
	}
	return out
}

// trackedEntry returns the registry entry for folder, matched
// case-insensitively, and whether one exists.
func (s *Service) trackedEntry(e *env, folder string) (catalog.Entry, bool) {
	entry, ok := s.registryEntries(e)[strings.ToLower(folder)]
	return entry, ok
}

// tocVersion returns the ## Version of the folder's first *.toc file,
// or "" when the folder has none or it cannot be read. Mirrors the
// catalog's private readTOCVersion.
func (s *Service) tocVersion(folder string) string {
	matches, err := filepath.Glob(filepath.Join(folder, "*.toc"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	toc, err := validator.ParseTOC(matches[0])
	if err != nil || toc.Version == "" {
		return ""
	}
	return toc.Version
}

// DTOs ---------------------------------------------------------------------

// AppState is the initial UI snapshot.
type AppState struct {
	Version       string `json:"version"`
	WoWPath       string `json:"wow_path"`
	Flavor        string `json:"flavor"`
	AddonsDir     string `json:"addons_dir"`
	ProfileID     string `json:"profile_id"`
	ProfileName   string `json:"profile_name"`
	HasInstall    bool   `json:"has_install"`
	AutoBackup    bool   `json:"auto_backup"`
	Confirmations bool   `json:"confirmations"`
}

// Install is one detected (or selected) WoW installation.
type Install struct {
	Root       string `json:"root"`
	Flavor     string `json:"flavor"`
	AddonsPath string `json:"addons_path"`
	Exe        string `json:"exe"`
	Version    string `json:"version"`
	ProfileID  string `json:"profile_id"`
	Confidence string `json:"confidence"`
}

func toInstall(i detector.Installation) Install {
	return Install{
		Root:       i.Root,
		Flavor:     i.Flavor,
		AddonsPath: i.AddonsPath,
		Exe:        i.Exe,
		Version:    i.Version,
		ProfileID:  i.ProfileID,
		Confidence: i.Confidence,
	}
}

// Profile is a supported game-version profile.
type Profile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Family    string `json:"family"`
	Interface int    `json:"interface"`
}

// TOC summarizes the primary TOC of an addon.
type TOC struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Interface int    `json:"interface"`
	Version   string `json:"version"`
}

// Issue is one problem attached to an addon.
type Issue struct {
	Kind          string   `json:"kind"`
	Severity      string   `json:"severity"`
	Message       string   `json:"message"`
	Suggestion    string   `json:"suggestion"`
	Action        string   `json:"action"`
	ActionLabel   string   `json:"action_label"`
	Options       []string `json:"options"`
	SuggestedName string   `json:"suggested_name"`
}

// Compat is a TOC-vs-profile compatibility verdict.
type Compat struct {
	FolderName string `json:"folder_name"`
	TOC        string `json:"toc"`
	Expected   int    `json:"expected"`
	Detected   int    `json:"detected"`
	Status     string `json:"status"`
	Label      string `json:"label"`
}

// Addon is one addon in the scan report.
type Addon struct {
	FolderName    string   `json:"folder_name"`
	BaseName      string   `json:"base_name"`
	SuggestedName string   `json:"suggested_name"`
	Status        string   `json:"status"`
	Nested        bool     `json:"nested"`
	SizeBytes     int64    `json:"size_bytes"`
	Fixable       bool     `json:"fixable"`
	Health        int      `json:"health"`
	TOC           *TOC     `json:"toc"`
	Issues        []Issue  `json:"issues"`
	Compat        []Compat `json:"compat"`
	// Tracked reports whether the addon was installed through the
	// catalog and is recorded in the registry. Drifted reports that a
	// tracked addon's folder no longer matches the manifest checksum
	// recorded at install/update time; entries without a recorded
	// checksum (pre-integrity installs) are never drifted.
	Tracked       bool   `json:"tracked"`
	Drifted       bool   `json:"drifted"`
	TrackedSource string `json:"tracked_source,omitempty"`
	// Pinned and Ignored mirror the registry entry's flags; both are
	// false for untracked addons.
	Pinned  bool `json:"pinned"`
	Ignored bool `json:"ignored"`
}

// Stats summarizes a scan.
type Stats struct {
	Total    int `json:"total"`
	Problems int `json:"problems"`
	Errors   int `json:"errors"`
}

// ScanResult is the full scan report.
type ScanResult struct {
	AddonsDir string    `json:"addons_dir"`
	ProfileID string    `json:"profile_id"`
	ScannedAt time.Time `json:"scanned_at"`
	Addons    []Addon   `json:"addons"`
	Errors    []string  `json:"errors"`
	Stats     Stats     `json:"stats"`
}

// ValidateResult is the per-addon compatibility table.
type ValidateResult struct {
	ProfileID string   `json:"profile_id"`
	Expected  int      `json:"expected"`
	Addons    []Compat `json:"addons"`
}

// Fix is the outcome of fixing one issue.
type Fix struct {
	Addon   string `json:"addon"`
	Action  string `json:"action"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// FixBatch is the outcome of a fix run.
type FixBatch struct {
	Fixes  []Fix `json:"fixes"`
	Fixed  int   `json:"fixed"`
	Failed int   `json:"failed"`
}

// InstallResult is the outcome of installing a ZIP archive.
type InstallResult struct {
	Installed []string `json:"installed"`
	Replaced  []string `json:"replaced"`
	Skipped   []string `json:"skipped"`
	Errors    []string `json:"errors"`
}

// WagoImportResult is the outcome of saving a WeakAuras/Plater import
// string from the Wago provider to disk.
type WagoImportResult struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Bytes       int    `json:"bytes"`
	AppliedHint string `json:"applied_hint"`
}

// UpdateInfo is one pending addon update.
type UpdateInfo struct {
	Folder         string `json:"folder"`
	Title          string `json:"title"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	Provider       string `json:"provider"`
	ID             string `json:"id"`
	Source         string `json:"source"`
	FlavorMismatch bool   `json:"flavor_mismatch"`
	FlavorLabel    string `json:"flavor_label"`
}

// UpdatesResult is the outcome of a catalog update check.
type UpdatesResult struct {
	Updates   []UpdateInfo `json:"updates"`
	Errors    []string     `json:"errors"`
	CheckedAt string       `json:"checked_at"`
}

// SnapshotResult is the outcome of exporting an offline catalog
// snapshot: the portable snapshot JSON plus a summary for the UI.
type SnapshotResult struct {
	SnapshotJSON string   `json:"snapshot_json"`
	ExportedAt   string   `json:"exported_at"`
	AddonCount   int      `json:"addon_count"`
	Warnings     []string `json:"warnings"`
}

// SnapshotCheck is the outcome of checking a catalog snapshot against
// the current registry without any network access.
type SnapshotCheck struct {
	Updates []UpdateInfo `json:"updates"`
	Errors  []string     `json:"errors"`
}

// TrackedAddon is one catalog-registry entry in the management view.
type TrackedAddon struct {
	Folder      string `json:"folder"`
	Title       string `json:"title"`
	Version     string `json:"version"`
	Provider    string `json:"provider"`
	ID          string `json:"id"`
	Source      string `json:"source"`
	Pinned      bool   `json:"pinned"`
	Ignored     bool   `json:"ignored"`
	InstalledAt string `json:"installed_at"`
	// HasHistory reports whether the addon has a recorded version log
	// (drives the UI's History entry state).
	HasHistory bool `json:"has_history"`
}

// AddonVersion is one recorded version of a tracked addon, newest
// first.
type AddonVersion struct {
	Version  string `json:"version"`
	Provider string `json:"provider,omitempty"`
	Source   string `json:"source,omitempty"`
	Ref      string `json:"ref,omitempty"`
	At       string `json:"at"`
}

// VersionHistoryResult is the recorded version log of one tracked
// addon, newest first, with the currently installed version.
type VersionHistoryResult struct {
	Folder   string         `json:"folder"`
	Current  string         `json:"current"`
	Versions []AddonVersion `json:"versions"`
}

// TrackedResult is the full tracked-addon list.
type TrackedResult struct {
	Addons []TrackedAddon `json:"addons"`
}

// RollbackResult is the outcome of rolling one addon back to a
// snapshot.
type RollbackResult struct {
	Folder       string `json:"folder"`
	RestoredFrom string `json:"restored_from"`
	Version      string `json:"version"`
	Pinned       bool   `json:"pinned"`
	Message      string `json:"message"`
}

// ApplyEntry is the outcome of applying one update.
type ApplyEntry struct {
	Folder  string `json:"folder"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// ApplyBatch is the outcome of applying one or more updates.
type ApplyBatch struct {
	Applied      []ApplyEntry `json:"applied"`
	AppliedCount int          `json:"applied_count"`
	FailedCount  int          `json:"failed_count"`
	// errors carries hard per-install failures (e.g. catalog setup)
	// that are not per-addon outcomes. It is not part of the JSON
	// contract; SyncUpdatesToAll consumes it for its per-install
	// error lists.
	errors []string
}

// CollectionInfo is one addon collection in the list view.
type CollectionInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AddonCount int    `json:"addon_count"`
	Active     bool   `json:"active"`
}

// CollectionsResult is the full collection list with the active id.
type CollectionsResult struct {
	Collections []CollectionInfo `json:"collections"`
	ActiveID    string           `json:"active_id"`
}

// CollectionAddon is one addon's desired state inside a collection.
type CollectionAddon struct {
	Folder  string `json:"folder"`
	Enabled bool   `json:"enabled"`
}

// CollectionDetail is one collection's full addon state table.
type CollectionDetail struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Addons []CollectionAddon `json:"addons"`
}

// SwitchResult is the outcome of activating a collection.
type SwitchResult struct {
	Applied []string `json:"applied"`
	Message string   `json:"message"`
}

// InstallStatus is one detected installation with live scan health.
type InstallStatus struct {
	Root       string `json:"root"`
	Flavor     string `json:"flavor"`
	AddonsPath string `json:"addons_path"`
	Exe        string `json:"exe"`
	Version    string `json:"version"`
	ProfileID  string `json:"profile_id"`
	Confidence string `json:"confidence"`
	Exists     bool   `json:"exists"`
	Addons     int    `json:"addons"`
	Problems   int    `json:"problems"`
	Errors     int    `json:"errors"`
	Health     int    `json:"health"`
}

// InstallsStatusResult is the status of every detected installation.
type InstallsStatusResult struct {
	Installs []InstallStatus `json:"installs"`
}

// SyncInstallResult is the outcome of a bulk update run for one install.
type SyncInstallResult struct {
	Root    string   `json:"root"`
	Updated int      `json:"updated"`
	Failed  int      `json:"failed"`
	Errors  []string `json:"errors"`
}

// SyncResult aggregates a cross-install bulk update run.
type SyncResult struct {
	Installs     []SyncInstallResult `json:"installs"`
	TotalUpdated int                 `json:"total_updated"`
	TotalFailed  int                 `json:"total_failed"`
}

// SearchHit is one addon found by a catalog search.
type SearchHit struct {
	Provider      string `json:"provider"`
	Name          string `json:"name"`
	Author        string `json:"author"`
	Summary       string `json:"summary"`
	LatestVersion string `json:"latest_version"`
	GameVersion   string `json:"game_version"`
	ID            string `json:"id"`
	Homepage      string `json:"homepage"`
}

// SearchResult is the outcome of a catalog search.
type SearchResult struct {
	Results []SearchHit `json:"results"`
	Errors  []string    `json:"errors"`
}

// CuratedAddon is one addon in a curated private-server set, with its
// live install state for the active installation.
type CuratedAddon struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	Summary          string `json:"summary"`
	Homepage         string `json:"homepage"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
}

// CuratedResult is the curated set for one game family.
type CuratedResult struct {
	Family    string         `json:"family"`
	Label     string         `json:"label"`
	ProfileID string         `json:"profile_id"`
	Addons    []CuratedAddon `json:"addons"`
}

// InfoResult is the resolved detail view of one addon: either a single
// addon or, for an ambiguous bare-name search, the candidate matches
// the frontend asks the user to disambiguate.
type InfoResult struct {
	Provider      string      `json:"provider"`
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Author        string      `json:"author"`
	Summary       string      `json:"summary"`
	LatestVersion string      `json:"latest_version"`
	Homepage      string      `json:"homepage"`
	GameVersion   string      `json:"game_version"`
	UpdatedAt     time.Time   `json:"updated_at"`
	ReleaseNotes  string      `json:"release_notes,omitempty"`
	Matches       []SearchHit `json:"matches,omitempty"`
}

// ProviderInfo is one catalog provider's name and honest caveat.
type ProviderInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DoctorCheck is one line of the environment report, with a machine
// status for the UI to color.
type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "ok" | "warn" | "error" | "info"
	Message string `json:"message"`
}

// DoctorReport is the full environment report.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
}

// SavedVarsListResult is one account's SavedVariables file list.
type SavedVarsListResult struct {
	WtfRoot string   `json:"wtf_root"`
	Account string   `json:"account"`
	Files   []string `json:"files"`
}

// SavedVarsBackupResult is the outcome of backing up one account.
type SavedVarsBackupResult struct {
	Path    string `json:"path"`
	Account string `json:"account"`
}

// SavedVarsMigrateResult is the outcome of copying SavedVariables
// between two accounts of the same installation.
type SavedVarsMigrateResult struct {
	Copied []string `json:"copied"`
}

// BackupResult is the outcome of a manual snapshot.
type BackupResult struct {
	ID string `json:"id"`
}

// BackupInfo is one snapshot in the backup history list.
type BackupInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Reason    string    `json:"reason"`
	Folders   int       `json:"folders"`
}

// ListBackupsResult is the full snapshot history.
type ListBackupsResult struct {
	Snapshots []BackupInfo `json:"snapshots"`
}

// RestoreBackupResult is the outcome of restoring one snapshot.
type RestoreBackupResult struct {
	Restored []string `json:"restored"`
	Skipped  []string `json:"skipped"`
}

// ExportResult is the outcome of exporting a collection.
type ExportResult struct {
	Out        string `json:"out"`
	Addons     int    `json:"addons"`
	Collection string `json:"collection"`
}

// ImportResult is the outcome of importing a collection.
type ImportResult struct {
	Installed []string `json:"installed"`
}

// ConfigView is the full persisted configuration.
type ConfigView struct {
	WoWPath          string `json:"wow_path"`
	Flavor           string `json:"flavor"`
	Profile          string `json:"profile"`
	Collection       string `json:"collection"`
	Theme            string `json:"theme"`
	AutoBackup       bool   `json:"auto_backup"`
	Confirmations    bool   `json:"confirmations"`
	BackupsDir       string `json:"backups_dir"`
	CurseForgeAPIKey string `json:"curseforge_api_key"`
	CollectionsDir   string `json:"collections_dir"`
}

// DTO conversions -----------------------------------------------------------

func (s *Service) toScanResult(e *env, res *models.ScanResult) ScanResult {
	out := ScanResult{
		AddonsDir: res.AddonsDir,
		ProfileID: e.profile.ID,
		ScannedAt: res.ScannedAt,
		Errors:    errStrings(res.Errors),
	}
	out.Stats.Total, out.Stats.Problems, out.Stats.Errors = res.Stats()
	tracked := s.registryEntries(e)
	out.Addons = make([]Addon, 0, len(res.Addons))
	for _, a := range res.Addons {
		ad := toAddon(a, e.profile)
		if entry, ok := tracked[strings.ToLower(a.FolderName)]; ok {
			ad.Tracked = true
			ad.TrackedSource = entry.Source
			ad.Pinned = entry.Pinned
			ad.Ignored = entry.Ignored
			if entry.Checksum != "" {
				if sum, err := catalog.ComputeManifest(filepath.Join(res.AddonsDir, a.FolderName)); err == nil {
					ad.Drifted = sum != entry.Checksum
				}
				// A manifest error (folder vanished mid-scan, unreadable
				// file) leaves Drifted false: best-effort, never fails
				// the scan.
			}
		}
		out.Addons = append(out.Addons, ad)
	}
	return out
}

// addonHealth derives a 0-100 health score from the addon's issues:
// 100 minus 30 per error, 15 per warn and 5 per info, clamped at 0.
// A clean addon scores 100.
func addonHealth(a *models.Addon) int {
	score := 100
	for _, i := range a.Issues {
		switch i.Severity {
		case models.SeverityError:
			score -= 30
		case models.SeverityWarn:
			score -= 15
		case models.SeverityInfo:
			score -= 5
		}
	}
	if score < 0 {
		return 0
	}
	return score
}

func toAddon(a *models.Addon, profile *models.Profile) Addon {
	ad := Addon{
		FolderName:    a.FolderName,
		BaseName:      a.BaseName,
		SuggestedName: a.SuggestedName,
		Status:        string(a.Status),
		Nested:        a.Nested,
		SizeBytes:     a.SizeBytes,
		Fixable:       a.Fixable(),
		Health:        addonHealth(a),
	}
	if pt := a.PrimaryTOC(); pt != nil {
		ad.TOC = &TOC{Name: pt.Name, Title: pt.Title, Interface: pt.Interface, Version: pt.Version}
	}
	for _, i := range a.Issues {
		ad.Issues = append(ad.Issues, Issue{
			Kind:          string(i.Kind),
			Severity:      string(i.Severity),
			Message:       i.Message,
			Suggestion:    i.Suggestion,
			Action:        string(i.Action),
			ActionLabel:   i.Action.Label(),
			Options:       i.Options,
			SuggestedName: i.SuggestedName,
		})
	}
	ad.Compat = append(ad.Compat, toCompat(a, profile))
	return ad
}

func toCompat(a *models.Addon, profile *models.Profile) Compat {
	c := classifyTOC(a, profile)
	detected, tocName := -1, ""
	if c.TOC != nil {
		tocName = c.TOC.Name
		detected = c.TOC.Interface
	}
	return Compat{
		FolderName: a.FolderName,
		TOC:        tocName,
		Expected:   profile.Interface,
		Detected:   detected,
		Status:     string(c.Status),
		Label:      c.Label,
	}
}

func toFixBatch(results []fixer.Result) FixBatch {
	batch := FixBatch{Fixes: make([]Fix, 0, len(results))}
	for _, r := range results {
		f := Fix{Addon: r.Addon, Action: r.Action, OK: r.OK, Message: r.Message}
		if r.Err != nil {
			f.Error = r.Err.Error()
		}
		batch.Fixes = append(batch.Fixes, f)
		if r.OK {
			batch.Fixed++
		} else {
			batch.Failed++
		}
	}
	return batch
}

func toInstallResult(res *installer.Result) InstallResult {
	out := InstallResult{
		Installed: res.Installed,
		Replaced:  res.Replaced,
		Skipped:   []string{},
		Errors:    errStrings(res.Errors),
	}
	for folder, reason := range res.Skipped {
		out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %s", folder, reason))
	}
	sort.Strings(out.Skipped)
	return out
}

// statusForInstall builds one install status row: scan stats and the
// average addon health when the AddOns directory exists, zeroes
// otherwise. A failed scan keeps exists=true and zeroes every count:
// one broken install never fails the surrounding call.
func (s *Service) statusForInstall(inst *detector.Installation, profile *models.Profile) InstallStatus {
	st := InstallStatus{
		Root:       inst.Root,
		Flavor:     inst.Flavor,
		AddonsPath: inst.AddonsPath,
		Exe:        inst.Exe,
		Version:    inst.Version,
		ProfileID:  inst.ProfileID,
		Confidence: inst.Confidence,
		Exists:     inst.Exists(),
	}
	if !st.Exists {
		return st
	}
	res, err := scanner.New(inst.AddonsPath, profile).Scan(context.Background())
	if err != nil {
		return st
	}
	st.Addons, st.Problems, st.Errors = res.Stats()
	if st.Addons > 0 {
		total := 0
		for _, a := range res.Addons {
			total += addonHealth(a)
		}
		st.Health = total / st.Addons
	}
	return st
}

// Wails-bound methods -------------------------------------------------------

// GetState returns the initial UI state, install or not. A saved
// wow_path that no longer resolves is not fatal: the state degrades to
// HasInstall=false and keeps the stale path so the setup view can
// prefill the path picker. Only a genuinely fatal problem (config store
// unreadable) surfaces as an error.
func (s *Service) GetState() (AppState, error) {
	cfg, err := s.store.Load()
	if err != nil {
		return AppState{}, err
	}
	if cfg.WoWPath == "" {
		// First-run adoption: when auto-detection finds an install,
		// adopt the best one (AutoDetect sorts best-confidence first),
		// persist it and report the full install state, so the setup
		// wizard only appears when nothing is detectable. The detected
		// install's profile wins on this path; an install with empty
		// ProfileID leaves the saved profile untouched.
		detect := s.autoDetect
		if detect == nil {
			detect = detector.AutoDetect
		}
		if installs, err := detect(context.Background()); err == nil && len(installs) > 0 {
			best := installs[0]
			cfg.WoWPath = best.Root
			cfg.Flavor = best.Flavor
			if best.ProfileID != "" {
				cfg.Profile = best.ProfileID
			}
			// A failed persist must not brick first-run boot: the
			// adoption still applies in memory and the error is
			// logged, not surfaced.
			if err := s.store.Save(cfg); err != nil {
				s.log.Errorf("auto-adopt: cannot persist install %q: %v", best.Root, err)
			}
			e := s.buildEnv(cfg, &best)
			return AppState{
				Version:       s.Version,
				WoWPath:       e.install.Root,
				Flavor:        e.install.Flavor,
				AddonsDir:     e.install.AddonsPath,
				ProfileID:     e.profile.ID,
				ProfileName:   e.profile.Name,
				AutoBackup:    e.cfg.AutoBackup,
				Confirmations: e.cfg.Confirmations,
				HasInstall:    true,
			}, nil
		}
		// Nothing detected (or detection failed): report the setup
		// state exactly as resolveInstall with no install would. Never
		// fall through to resolveInstall here — it would re-run
		// detection against the real machine.
		e := s.buildEnv(cfg, nil)
		return AppState{
			Version:       s.Version,
			WoWPath:       cfg.WoWPath,
			ProfileID:     e.profile.ID,
			ProfileName:   e.profile.Name,
			AutoBackup:    e.cfg.AutoBackup,
			Confirmations: e.cfg.Confirmations,
		}, nil
	}
	install, err := s.resolveInstall(cfg)
	if err != nil || install == nil {
		// No usable install (stale saved path, nothing auto-detected):
		// report the setup state instead of failing the whole UI.
		e := s.buildEnv(cfg, nil)
		return AppState{
			Version:       s.Version,
			WoWPath:       cfg.WoWPath,
			ProfileID:     e.profile.ID,
			ProfileName:   e.profile.Name,
			AutoBackup:    e.cfg.AutoBackup,
			Confirmations: e.cfg.Confirmations,
		}, nil
	}
	e := s.buildEnv(cfg, install)
	st := AppState{
		Version:       s.Version,
		ProfileID:     e.profile.ID,
		ProfileName:   e.profile.Name,
		AutoBackup:    e.cfg.AutoBackup,
		Confirmations: e.cfg.Confirmations,
	}
	st.WoWPath = e.install.Root
	st.Flavor = e.install.Flavor
	st.AddonsDir = e.install.AddonsPath
	st.HasInstall = true
	return st, nil
}

// DetectInstalls auto-detects every WoW installation on the host.
func (s *Service) DetectInstalls() ([]Install, error) {
	installs, err := detector.AutoDetect(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]Install, 0, len(installs))
	for _, i := range installs {
		out = append(out, toInstall(i))
	}
	return out, nil
}

// SetInstall selects the installation at root and persists it. When
// flavor is non-empty it overrides the detected flavor.
func (s *Service) SetInstall(root, flavor string) (Install, error) {
	inst, err := detector.DetectPath(root)
	if err != nil {
		return Install{}, err
	}
	if flavor != "" {
		inst.Flavor = flavor
		inst.AddonsPath = detector.AddonsPath(inst.Root, flavor)
	}
	cfg, err := s.store.Load()
	if err != nil {
		return Install{}, err
	}
	cfg.WoWPath = inst.Root
	cfg.Flavor = inst.Flavor
	if inst.ProfileID != "" {
		cfg.Profile = inst.ProfileID
	}
	if err := s.store.Save(cfg); err != nil {
		return Install{}, err
	}
	return toInstall(*inst), nil
}

// SetProfile persists the active game-version profile.
func (s *Service) SetProfile(id string) error {
	p := models.ProfileByID(id)
	if p == nil {
		return fmt.Errorf("unknown profile %q", id)
	}
	cfg, err := s.store.Load()
	if err != nil {
		return err
	}
	cfg.Profile = p.ID
	return s.store.Save(cfg)
}

// Profiles lists every supported game-version profile.
func (s *Service) Profiles() ([]Profile, error) {
	out := make([]Profile, 0, len(models.Profiles))
	for _, p := range models.Profiles {
		out = append(out, Profile{ID: p.ID, Name: p.Name, Family: p.Family, Interface: p.Interface})
	}
	return out, nil
}

// Scan runs a fresh scan of the selected installation.
func (s *Service) Scan() (ScanResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return ScanResult{}, err
	}
	res, err := s.scan(e)
	if err != nil {
		return ScanResult{}, err
	}
	return s.toScanResult(e, res), nil
}

// Validate reports the TOC compatibility table for every addon.
func (s *Service) Validate() (ValidateResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return ValidateResult{}, err
	}
	res, err := s.scan(e)
	if err != nil {
		return ValidateResult{}, err
	}
	out := ValidateResult{ProfileID: e.profile.ID, Expected: e.profile.Interface}
	out.Addons = make([]Compat, 0, len(res.Addons))
	for _, a := range res.Addons {
		out.Addons = append(out.Addons, toCompat(a, e.profile))
	}
	return out, nil
}

// Fix repairs every issue of one addon. allowDestructive stands in for
// the user's confirmation of destructive steps.
func (s *Service) Fix(folderName string, allowDestructive bool) (FixBatch, error) {
	e, err := s.requireInstall()
	if err != nil {
		return FixBatch{}, err
	}
	res, err := s.scan(e)
	if err != nil {
		return FixBatch{}, err
	}
	var addon *models.Addon
	for _, a := range res.Addons {
		if strings.EqualFold(a.FolderName, folderName) {
			addon = a
			break
		}
	}
	if addon == nil {
		return FixBatch{}, fmt.Errorf("addon %q not found", folderName)
	}
	f := fixer.New(s.fixerOptions(e, allowDestructive))
	return toFixBatch(f.Fix(context.Background(), addon)), nil
}

// FixAll repairs every fixable addon. allowDestructive stands in for
// the user's confirmation of destructive steps.
func (s *Service) FixAll(allowDestructive bool) (FixBatch, error) {
	e, err := s.requireInstall()
	if err != nil {
		return FixBatch{}, err
	}
	res, err := s.scan(e)
	if err != nil {
		return FixBatch{}, err
	}
	f := fixer.New(s.fixerOptions(e, allowDestructive))
	return toFixBatch(f.FixAll(context.Background(), res.Addons)), nil
}

// InstallZip installs an addon archive. allowReplace decides whether
// existing folders may be replaced.
func (s *Service) InstallZip(zipPath string, allowReplace bool) (InstallResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return InstallResult{}, err
	}
	if _, err := detector.EnsureAddons(e.install); err != nil {
		return InstallResult{}, err
	}
	res, err := installer.New(s.installerOptions(e, allowReplace)).Install(context.Background(), zipPath)
	if err != nil {
		return InstallResult{}, err
	}
	return toInstallResult(res), nil
}

// CheckUpdates compares every registry-tracked addon against its
// provider and reports the available updates. Partial provider
// failures land in Errors while the found updates are still returned;
// only hard failures (no install, unreadable registry) surface as the
// error.
func (s *Service) CheckUpdates() (UpdatesResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return UpdatesResult{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return UpdatesResult{}, err
	}
	updates, checkErr := catalog.Check(context.Background(), cat, cat.Reg, e.install.AddonsPath)
	out := UpdatesResult{
		Errors:    []string{},
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, u := range updates {
		info := UpdateInfo{
			Folder:         u.Entry.Folder,
			Title:          u.Entry.Title,
			CurrentVersion: u.Entry.Version,
			LatestVersion:  u.Latest.LatestVersion,
			Provider:       u.Entry.Provider,
			ID:             u.Entry.ID,
			Source:         u.Entry.Source,
			FlavorMismatch: u.Mismatch,
		}
		if u.Mismatch {
			info.FlavorLabel = flavorLabel(u.Latest.GameVersion, e.profile)
		}
		out.Updates = append(out.Updates, info)
	}
	if checkErr != nil {
		out.Errors = append(out.Errors, checkErr.Error())
	}
	return out, nil
}

// ExportSnapshot freezes the tracked addon state with the latest known
// versions into a portable JSON snapshot, for offline update checks.
// Pinned and ignored addons are included with their flags; per-addon
// resolution failures land in Warnings while the export continues.
// Requires an install (it resolves the active profile through it).
func (s *Service) ExportSnapshot() (SnapshotResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return SnapshotResult{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return SnapshotResult{}, err
	}
	snap, err := catalog.ExportSnapshot(context.Background(), cat, cat.Reg, e.profile.ID, time.Now())
	if err != nil {
		return SnapshotResult{}, err
	}
	data, err := snap.Marshal()
	if err != nil {
		return SnapshotResult{}, err
	}
	warnings := snap.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	return SnapshotResult{
		SnapshotJSON: string(data),
		ExportedAt:   snap.ExportedAt.Format(time.RFC3339),
		AddonCount:   len(snap.Addons),
		Warnings:     warnings,
	}, nil
}

// CheckSnapshot diffs a snapshot's latest versions against the current
// registry and reports the pending updates entirely offline: no
// provider is queried. Invalid snapshot JSON is a clear Go error.
// Flavor mismatches are computed against the profile recorded in the
// snapshot, and the snapshot's export warnings surface in Errors.
func (s *Service) CheckSnapshot(snapshotJSON string) (SnapshotCheck, error) {
	snap, err := catalog.UnmarshalSnapshot([]byte(snapshotJSON))
	if err != nil {
		return SnapshotCheck{}, err
	}
	e, err := s.requireInstall()
	if err != nil {
		return SnapshotCheck{}, err
	}
	reg, err := s.loadRegistry(e)
	if err != nil {
		return SnapshotCheck{}, err
	}
	updates, err := snap.Diff(reg)
	if err != nil {
		return SnapshotCheck{}, err
	}
	profile := models.ProfileByID(snap.Profile)
	out := SnapshotCheck{Updates: []UpdateInfo{}, Errors: []string{}}
	for _, u := range updates {
		info := UpdateInfo{
			Folder:         u.Entry.Folder,
			Title:          u.Entry.Title,
			CurrentVersion: u.Entry.Version,
			LatestVersion:  u.Latest.LatestVersion,
			Provider:       u.Entry.Provider,
			ID:             u.Entry.ID,
			Source:         u.Entry.Source,
			FlavorMismatch: u.Mismatch,
		}
		if u.Mismatch {
			info.FlavorLabel = flavorLabel(u.Latest.GameVersion, profile)
		}
		out.Updates = append(out.Updates, info)
	}
	out.Errors = append(out.Errors, snap.Warnings...)
	return out, nil
}

// ApplyUpdate applies the pending update for one folder, matched
// case-insensitively against a fresh check. allowReplace stands in
// for the user's confirmation: without it an update that would
// replace an existing folder is skipped with a message.
func (s *Service) ApplyUpdate(folder string, allowReplace bool) (ApplyBatch, error) {
	e, err := s.requireInstall()
	if err != nil {
		return ApplyBatch{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return ApplyBatch{}, err
	}
	updates, _ := catalog.Check(context.Background(), cat, cat.Reg, e.install.AddonsPath)
	var target *catalog.Update
	for i := range updates {
		if strings.EqualFold(updates[i].Entry.Folder, folder) {
			target = &updates[i]
			break
		}
	}
	if target == nil {
		return ApplyBatch{
			Applied:     []ApplyEntry{{Folder: folder, Message: fmt.Sprintf("no update available for %q", folder)}},
			FailedCount: 1,
		}, nil
	}
	if !allowReplace && folderExists(e.install.AddonsPath, target.Entry.Folder) {
		return ApplyBatch{
			Applied:     []ApplyEntry{{Folder: folder, Message: "folder already exists, replace declined"}},
			FailedCount: 1,
		}, nil
	}
	installed, err := catalog.Apply(context.Background(), cat, e.install.AddonsPath, *target, cat.Backups, s.log)
	if err != nil {
		return ApplyBatch{
			Applied:     []ApplyEntry{{Folder: folder, Message: err.Error(), Error: err.Error()}},
			FailedCount: 1,
		}, nil
	}
	return ApplyBatch{
		Applied:      []ApplyEntry{{Folder: installed, OK: true, Message: "applied"}},
		AppliedCount: 1,
	}, nil
}

// ApplyAllUpdates applies every pending update and collects the
// outcomes into one batch. A failure never stops the remaining
// updates.
func (s *Service) ApplyAllUpdates(allowReplace bool) (ApplyBatch, error) {
	e, err := s.requireInstall()
	if err != nil {
		return ApplyBatch{}, err
	}
	return s.applyAllIn(e, e.install.AddonsPath, allowReplace), nil
}

// checkAllIn resolves the catalog for the environment and checks the
// tracked addons against one AddOns directory. Check is read-only, so
// several installs can be checked against the same registry state
// before any update is applied. A catalog setup failure returns the
// error in the string list with nil updates. Check's own error is
// deliberately dropped, matching the single-install path: provider
// failures surface per-entry as skipped updates, never as a hard
// failure.
func (s *Service) checkAllIn(e *env, addonsDir string) ([]catalog.Update, []string) {
	cat, err := s.catalogFor(e)
	if err != nil {
		return nil, []string{err.Error()}
	}
	updates, _ := catalog.Check(context.Background(), cat, cat.Reg, addonsDir)
	return updates, nil
}

// applyUpdates applies a pre-collected set of updates into one AddOns
// directory, snapshotting that install separately via backupRootFor. A
// catalog setup failure lands in the batch's errors field with zero
// counts; per-update failures are counted and never stop the rest.
func (s *Service) applyUpdates(e *env, addonsDir string, updates []catalog.Update, allowReplace bool) ApplyBatch {
	cat, err := s.catalogFor(e)
	if err != nil {
		return ApplyBatch{Applied: []ApplyEntry{}, errors: []string{err.Error()}}
	}
	backups := backup.New(s.backupRootFor(e, addonsDir), s.log)
	batch := ApplyBatch{Applied: []ApplyEntry{}}
	for _, u := range updates {
		entry := ApplyEntry{Folder: u.Entry.Folder}
		if !allowReplace && folderExists(addonsDir, u.Entry.Folder) {
			entry.Message = "folder already exists, replace declined"
			batch.Applied = append(batch.Applied, entry)
			batch.FailedCount++
			continue
		}
		installed, err := catalog.Apply(context.Background(), cat, addonsDir, u, backups, s.log)
		if err != nil {
			entry.Message = err.Error()
			entry.Error = err.Error()
			batch.Applied = append(batch.Applied, entry)
			batch.FailedCount++
			continue
		}
		entry.OK = true
		entry.Folder = installed
		entry.Message = "applied"
		batch.Applied = append(batch.Applied, entry)
		batch.AppliedCount++
	}
	return batch
}

// applyAllIn runs the shared apply-all body against one AddOns
// directory: check the tracked addons, then apply every pending
// update. This is the single-install composition (one check followed
// by its own applies); cross-install sync uses checkAllIn and
// applyUpdates separately so every install's check sees the same
// registry baseline.
func (s *Service) applyAllIn(e *env, addonsDir string, allowReplace bool) ApplyBatch {
	updates, errs := s.checkAllIn(e, addonsDir)
	if errs != nil {
		return ApplyBatch{Applied: []ApplyEntry{}, errors: errs}
	}
	return s.applyUpdates(e, addonsDir, updates, allowReplace)
}

// SearchCatalog queries every enabled provider with the same query
// and returns the merged results. Partial provider failures land in
// Errors; when every provider fails the error is returned alongside
// the empty results.
func (s *Service) SearchCatalog(query string) (SearchResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return SearchResult{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return SearchResult{}, err
	}
	addons, searchErr := cat.Search(context.Background(), query, 20)
	out := SearchResult{Results: []SearchHit{}, Errors: []string{}}
	for _, a := range addons {
		out.Results = append(out.Results, SearchHit{
			Provider:      a.Provider,
			Name:          a.Name,
			Author:        a.Author,
			Summary:       a.Summary,
			LatestVersion: a.LatestVersion,
			GameVersion:   a.GameVersion,
			ID:            a.ID,
			Homepage:      a.Homepage,
		})
	}
	if searchErr != nil {
		out.Errors = append(out.Errors, searchErr.Error())
		if len(out.Results) == 0 {
			return out, searchErr
		}
	}
	return out, nil
}

// Curated returns the curated private-server addon set for the active
// profile's family, annotated with install state when an installation
// is available. Browsing must work without an install, so the env is
// resolved with env() rather than requireInstall. A profile family
// with no curated set (tbc, cata, retail) yields an empty list with a
// nil error; the frontend shows nothing.
func (s *Service) Curated() (CuratedResult, error) {
	e, err := s.env()
	if err != nil {
		return CuratedResult{}, err
	}
	m, err := catalog.LoadCurated()
	if err != nil {
		return CuratedResult{}, err
	}
	family, profileID := "", ""
	if e.profile != nil {
		family = e.profile.Family
		profileID = e.profile.ID
	}
	out := CuratedResult{Family: family, ProfileID: profileID, Addons: []CuratedAddon{}}
	set, ok := m.SetForFamily(family)
	if !ok {
		return out, nil
	}
	out.Label = set.Label
	for _, a := range set.Addons {
		row := CuratedAddon{Name: a.Name, Source: a.Source, Summary: a.Summary, Homepage: a.Homepage}
		if e.install != nil {
			if folder := curatedFolder(e.install.AddonsPath, a); folder != "" {
				row.Installed = true
				row.InstalledVersion = s.tocVersion(filepath.Join(e.install.AddonsPath, folder))
			}
		}
		out.Addons = append(out.Addons, row)
	}
	return out, nil
}

// curatedFolder returns the on-disk folder (case-insensitively) under
// addonsDir that a curated addon installs to, or "" when none is
// installed. Several curated sources ship under a bare folder that
// strips the repo's flavor suffix ("pfQuest-turtle" installs
// "pfQuest"), so a folder also matches when it is the target's prefix
// up to a "-" or "_" boundary.
func curatedFolder(addonsDir string, a catalog.CuratedAddon) string {
	entries, err := os.ReadDir(addonsDir)
	if err != nil {
		return ""
	}
	targets := []string{a.Name, curatedRepoBase(a.Source)}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, t := range targets {
			if curatedFolderMatch(e.Name(), t) {
				return e.Name()
			}
		}
	}
	return ""
}

func curatedFolderMatch(folder, target string) bool {
	if folder == "" || target == "" {
		return false
	}
	f := strings.ToLower(folder)
	t := strings.ToLower(target)
	if f == t {
		return true
	}
	if len(f) < len(t) && strings.HasPrefix(t, f) {
		return t[len(f)] == '-' || t[len(f)] == '_'
	}
	return false
}

// curatedRepoBase extracts the repo name from an "owner/repo" source.
func curatedRepoBase(source string) string {
	if i := strings.LastIndex(source, "/"); i >= 0 {
		return source[i+1:]
	}
	return source
}

// InstallSource installs an addon from a URL or provider-scoped id
// through the catalog (see catalog.InstallFromSource for the accepted
// source forms) and reports the outcome with the same DTO as
// InstallZip. The catalog layer reports installed folder names and
// errors only, so replaced/skipped stay empty; allowReplace is
// accepted for the frontend contract, but the catalog currently
// always replaces existing folders.
func (s *Service) InstallSource(source string, allowReplace bool) (InstallResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return InstallResult{}, err
	}
	return s.installSource(e, source, allowReplace)
}

// installSource runs the shared catalog install body: resolve the
// catalog for the environment, ensure the AddOns directory exists and
// install from source, mapping the outcome to the InstallResult DTO.
// Provider failures land in Errors with a nil Go error so the frontend
// renders them inline.
func (s *Service) installSource(e *env, source string, allowReplace bool) (InstallResult, error) {
	cat, err := s.catalogFor(e)
	if err != nil {
		return InstallResult{}, err
	}
	if _, err := detector.EnsureAddons(e.install); err != nil {
		return InstallResult{}, err
	}
	installed, err := cat.InstallFromSource(context.Background(), source, e.install.AddonsPath, nil)
	if err != nil {
		return InstallResult{
			Installed: []string{},
			Replaced:  []string{},
			Skipped:   []string{},
			Errors:    []string{err.Error()},
		}, nil
	}
	return InstallResult{
		Installed: installed,
		Replaced:  []string{},
		Skipped:   []string{},
		Errors:    []string{},
	}, nil
}

// SaveWagoImport downloads one Wago import (its 8-character slug) as
// the raw encoded import string and saves it to a WagoImports folder
// under the user Downloads directory (falling back to the config
// directory when Downloads is unavailable). The file is NOT an addon
// archive: it must be imported in-game (WeakAuras / Plater import
// panel), which the returned AppliedHint spells out.
func (s *Service) SaveWagoImport(id string) (WagoImportResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return WagoImportResult{}, err
	}
	if strings.TrimSpace(id) == "" {
		return WagoImportResult{}, fmt.Errorf("missing wago import id")
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return WagoImportResult{}, err
	}
	prov, ok := cat.Provider(catalog.ProviderWago)
	if !ok {
		return WagoImportResult{}, fmt.Errorf("wago provider is not enabled")
	}
	ctx := context.Background()
	addon, err := prov.Resolve(ctx, id)
	if err != nil {
		return WagoImportResult{}, err
	}
	base := sanitizeImportName(addon.Name)
	dir := s.wagoDir(e)
	dest := filepath.Join(dir, base+".txt")
	for n := 2; utils.Exists(dest); n++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s-%d.txt", base, n))
	}
	if err := prov.Download(ctx, addon, dest, nil); err != nil {
		return WagoImportResult{}, fmt.Errorf("wago: %w", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		return WagoImportResult{}, err
	}
	return WagoImportResult{
		Path:        dest,
		Name:        addon.Name,
		Bytes:       int(info.Size()),
		AppliedHint: fmt.Sprintf("Saved to %s — import it in-game via WeakAuras → Import", dest),
	}, nil
}

// wagoDir returns the folder Wago imports are saved to: the user
// Downloads directory when present, else the config directory. The
// WagoImports subfolder is always ensured (a failure surfaces later as
// a Download error).
func (s *Service) wagoDir(e *env) string {
	dir := s.wagoDirOverride
	if dir == "" {
		// The Downloads folder is <home>/Downloads on every supported
		// platform; there is no stdlib helper for it. Stat it so a
		// missing (or redirected) folder falls back to the config dir.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dl := filepath.Join(home, "Downloads")
			if info, err := os.Stat(dl); err == nil && info.IsDir() {
				dir = dl
			}
		}
	}
	if dir == "" {
		dir = e.store.Dir()
	}
	imports := filepath.Join(dir, "WagoImports")
	_ = utils.EnsureDir(imports)
	return imports
}

// sanitizeImportName reduces an addon name to filesystem-safe
// characters ([A-Za-z0-9._-]), collapsing runs of anything else to a
// single underscore. An empty result becomes "import".
func sanitizeImportName(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "import"
	}
	return out
}

// RestoreAddon re-downloads a tracked addon from the provider source
// recorded in the registry and replaces its folder, restoring the
// pristine manifest the registry's checksum records. The catalog layer
// re-records the fresh checksum after a successful install. An
// untracked folder is reported in the DTO's Errors with a nil Go
// error, mirroring InstallSource's error-handling style.
func (s *Service) RestoreAddon(folder string, allowReplace bool) (InstallResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return InstallResult{}, err
	}
	entry, ok := s.trackedEntry(e, folder)
	if !ok {
		return InstallResult{
			Installed: []string{},
			Replaced:  []string{},
			Skipped:   []string{},
			Errors:    []string{"addon not tracked in registry"},
		}, nil
	}
	return s.installSource(e, entry.Source, allowReplace)
}

// TrackedAddons lists every addon recorded in the catalog registry,
// sorted by folder, for the active install. An empty registry yields
// an empty list with a nil error.
func (s *Service) TrackedAddons() (TrackedResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return TrackedResult{}, err
	}
	reg, err := s.loadRegistry(e)
	if err != nil {
		return TrackedResult{}, err
	}
	out := TrackedResult{Addons: []TrackedAddon{}}
	for _, ent := range reg.Entries() { // already sorted by folder
		out.Addons = append(out.Addons, TrackedAddon{
			Folder:      ent.Folder,
			Title:       ent.Title,
			Version:     ent.Version,
			Provider:    ent.Provider,
			ID:          ent.ID,
			Source:      ent.Source,
			Pinned:      ent.Pinned,
			Ignored:     ent.Ignored,
			InstalledAt: ent.InstalledAt.UTC().Format(time.RFC3339),
			HasHistory:  len(ent.History) > 0,
		})
	}
	return out, nil
}

// SetAddonPinned locks or unlocks a tracked addon at its current
// version: pinned addons are skipped by update checks until unpinned.
// An untracked folder is an error.
func (s *Service) SetAddonPinned(folder string, pinned bool) error {
	e, err := s.requireInstall()
	if err != nil {
		return err
	}
	reg, err := s.loadRegistry(e)
	if err != nil {
		return err
	}
	return reg.SetPinned(folder, pinned)
}

// SetAddonIgnored includes or excludes a tracked addon from update
// management: ignored addons never appear in update checks. An
// untracked folder is an error.
func (s *Service) SetAddonIgnored(folder string, ignored bool) error {
	e, err := s.requireInstall()
	if err != nil {
		return err
	}
	reg, err := s.loadRegistry(e)
	if err != nil {
		return err
	}
	return reg.SetIgnored(folder, ignored)
}

// RollbackAddon restores the addon's folder from the newest backup
// snapshot that contains it (the current state is snapshotted first),
// then refreshes the registry entry from the restored content: the TOC
// version (falling back to the previously tracked version) and a fresh
// best-effort checksum. The entry is pinned afterwards — instawow
// semantics: the addon stops receiving updates until unpinned — while
// keeping its provider/source, so unpinning later resumes updates from
// the same source. Untracked folders and missing snapshots are Go
// errors, unlike RestoreAddon's DTO-error style, so the frontend can
// surface them distinctly.
func (s *Service) RollbackAddon(folder string) (RollbackResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return RollbackResult{}, err
	}
	entry, ok := s.trackedEntry(e, folder)
	if !ok {
		return RollbackResult{}, fmt.Errorf("addon %q not tracked in registry", folder)
	}
	backups := backup.New(s.backupRootFor(e, e.install.AddonsPath), s.log)
	dest := filepath.Join(e.install.AddonsPath, entry.Folder)
	restoredFrom, err := backups.RollbackFolder(dest)
	if err != nil {
		return RollbackResult{}, err
	}

	// Refresh the entry from the restored folder, mirroring the
	// install/update registration: TOC version first, then a
	// best-effort checksum (empty on manifest failure).
	version := s.tocVersion(dest)
	if version == "" {
		version = entry.Version
	}
	checksum, _ := catalog.ComputeManifest(dest)
	entry.Version = version
	entry.Checksum = checksum
	entry.Pinned = true
	reg, err := s.loadRegistry(e)
	if err != nil {
		return RollbackResult{}, err
	}
	if err := reg.Track(entry); err != nil {
		return RollbackResult{}, err
	}
	return RollbackResult{
		Folder:       entry.Folder,
		RestoredFrom: restoredFrom,
		Version:      version,
		Pinned:       true,
		Message:      fmt.Sprintf("restored from snapshot %s and pinned", restoredFrom),
	}, nil
}

// ListAddonVersions returns the recorded version history of one
// tracked addon, newest first, plus the currently installed version.
// An untracked folder is an error.
func (s *Service) ListAddonVersions(folder string) (VersionHistoryResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return VersionHistoryResult{}, err
	}
	entry, ok := s.trackedEntry(e, folder)
	if !ok {
		return VersionHistoryResult{}, fmt.Errorf("addon %q not tracked in registry", folder)
	}
	versions := make([]AddonVersion, 0, len(entry.History))
	for _, h := range entry.History {
		versions = append(versions, AddonVersion{
			Version:  h.Version,
			Provider: h.Provider,
			Source:   h.Source,
			Ref:      h.Ref,
			At:       h.At.UTC().Format(time.RFC3339),
		})
	}
	return VersionHistoryResult{Folder: entry.Folder, Current: entry.Version, Versions: versions}, nil
}

// RollbackToVersion re-downloads the specific past version of a
// tracked addon from its provider and replaces the folder, mirroring
// the update path: the current folder is snapshotted first and the
// registry entry is re-recorded with a fresh history entry. The
// version must be addressable by the provider — GitHub tags and
// CurseForge files are; WowInterface and Tukui serve only the latest
// and fail with an error wrapping catalog.ErrVersionNotServed — so a
// rollback never silently installs the latest release. Unknown
// folders and versions without a recorded history entry are Go
// errors.
func (s *Service) RollbackToVersion(folder, version string) (InstallResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return InstallResult{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return InstallResult{}, err
	}
	entry, ok := s.trackedEntry(e, folder)
	if !ok {
		return InstallResult{}, fmt.Errorf("addon %q not tracked in registry", folder)
	}
	var hist *catalog.VersionHistory
	for i := range entry.History {
		if entry.History[i].Version == version {
			hist = &entry.History[i]
			break
		}
	}
	if hist == nil {
		return InstallResult{}, fmt.Errorf("no recorded version %q for %q", version, folder)
	}
	backups := backup.New(s.backupRootFor(e, e.install.AddonsPath), s.log)
	installed, err := catalog.RollbackToVersion(context.Background(), cat, e.install.AddonsPath, entry, *hist, backups, s.log)
	if err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Installed: []string{installed}, Replaced: []string{}, Skipped: []string{}, Errors: []string{}}, nil
}

// Collections lists every collection with its addon count and marks
// the configured one active. The active id comes from the saved
// config, not from the on-disk folder state.
func (s *Service) Collections() (CollectionsResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return CollectionsResult{}, err
	}
	m, err := s.profilesFor(e)
	if err != nil {
		return CollectionsResult{}, err
	}
	cols, err := m.List()
	if err != nil {
		return CollectionsResult{}, err
	}
	out := CollectionsResult{
		Collections: []CollectionInfo{},
		ActiveID:    e.cfg.Collection,
	}
	for _, c := range cols {
		out.Collections = append(out.Collections, CollectionInfo{
			ID:         c.ID,
			Name:       c.Name,
			AddonCount: len(c.Addons),
			Active:     c.ID == e.cfg.Collection,
		})
	}
	return out, nil
}

// CreateCollection snapshots the current on-disk addon state into a
// new collection. The collection is not activated; the frontend
// decides whether to switch to it.
func (s *Service) CreateCollection(name string) (CollectionInfo, error) {
	e, err := s.requireInstall()
	if err != nil {
		return CollectionInfo{}, err
	}
	m, err := s.profilesFor(e)
	if err != nil {
		return CollectionInfo{}, err
	}
	c, err := m.Create(name)
	if err != nil {
		return CollectionInfo{}, err
	}
	return CollectionInfo{ID: c.ID, Name: c.Name, AddonCount: len(c.Addons)}, nil
}

// SwitchCollection applies the collection's addon state on disk and
// persists it as the active collection. The switch renames folders
// between "<name>" and "<name>.disabled" and is backup-snapshotted
// when auto_backup is on; the frontend's dialog is the confirmation
// gate. A missing collection is a Go error.
func (s *Service) SwitchCollection(id string) (SwitchResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return SwitchResult{}, err
	}
	m, err := s.profilesFor(e)
	if err != nil {
		return SwitchResult{}, err
	}
	applied, err := m.SwitchTo(id)
	if err != nil {
		return SwitchResult{}, err
	}
	name := id
	if c, err := m.Get(id); err == nil {
		name = c.Name
	}
	if e.cfg.Collection != id {
		e.cfg.Collection = id
		if err := s.store.Save(e.cfg); err != nil {
			return SwitchResult{}, err
		}
	}
	return SwitchResult{
		Applied: applied,
		Message: fmt.Sprintf("Switched to collection %q (%d addon(s) applied)", name, len(applied)),
	}, nil
}

// DeleteCollection removes a collection. Installed addons are
// untouched; the active collection id is cleared from the saved
// config when the deleted collection was active.
func (s *Service) DeleteCollection(id string) error {
	e, err := s.requireInstall()
	if err != nil {
		return err
	}
	m, err := s.profilesFor(e)
	if err != nil {
		return err
	}
	if err := m.Delete(id); err != nil {
		return err
	}
	if e.cfg.Collection == id {
		e.cfg.Collection = ""
		if err := s.store.Save(e.cfg); err != nil {
			return err
		}
	}
	return nil
}

// CollectionDetail loads one collection's full addon state table.
func (s *Service) CollectionDetail(id string) (CollectionDetail, error) {
	e, err := s.requireInstall()
	if err != nil {
		return CollectionDetail{}, err
	}
	m, err := s.profilesFor(e)
	if err != nil {
		return CollectionDetail{}, err
	}
	c, err := m.Get(id)
	if err != nil {
		return CollectionDetail{}, err
	}
	out := CollectionDetail{ID: c.ID, Name: c.Name}
	out.Addons = make([]CollectionAddon, 0, len(c.Addons))
	for _, a := range c.Addons {
		out.Addons = append(out.Addons, CollectionAddon{Folder: a.Folder, Enabled: a.Enabled})
	}
	return out, nil
}

// SetCollectionAddon toggles one addon's desired state in a
// collection. Unknown folders are appended.
func (s *Service) SetCollectionAddon(id, folder string, enabled bool) error {
	e, err := s.requireInstall()
	if err != nil {
		return err
	}
	m, err := s.profilesFor(e)
	if err != nil {
		return err
	}
	return m.SetEnabled(id, folder, enabled)
}

// InstallsStatus reports every detected installation with live scan
// stats and average addon health. Every install is inspected, not
// just the active one; a failed scan for one install keeps exists
// true and zeroes its counts instead of failing the whole call.
func (s *Service) InstallsStatus() (InstallsStatusResult, error) {
	installs, err := detector.AutoDetect(context.Background())
	if err != nil {
		return InstallsStatusResult{}, err
	}
	out := InstallsStatusResult{Installs: []InstallStatus{}}
	for i := range installs {
		profile := models.ProfileByID(installs[i].ProfileID)
		if profile == nil {
			profile = models.DefaultProfile()
		}
		out.Installs = append(out.Installs, s.statusForInstall(&installs[i], profile))
	}
	return out, nil
}

// SyncUpdatesToAll applies every pending update to every detected
// install with an existing AddOns directory: all installs are checked
// against the shared per-user registry first, then each install's
// updates are applied, so a tracked addon present in several installs
// is updated in each one. Every install is snapshotted separately. A
// failing install lands in its row's errors with zero counts and
// never aborts the remaining installs. No installs yields an empty
// result with a nil error.
func (s *Service) SyncUpdatesToAll(allowReplace bool) (SyncResult, error) {
	installs, err := detector.AutoDetect(context.Background())
	if err != nil {
		return SyncResult{}, err
	}
	e, err := s.env()
	if err != nil {
		return SyncResult{}, err
	}
	return s.syncInstalls(e, installs, allowReplace), nil
}

// syncInstalls runs the shared cross-install update body against an
// explicit install list (the tests inject one; SyncUpdatesToAll
// passes AutoDetect's results). It is two-pass on purpose: every
// install is checked FIRST against the same pre-apply registry state
// (Check is read-only; catalog.Apply re-records bumped versions), then
// each install's collected updates are applied. A tracked addon present
// in several installs is therefore updated in every one of them.
func (s *Service) syncInstalls(e *env, installs []detector.Installation, allowReplace bool) SyncResult {
	// Pass 1: check every install with an existing AddOns dir. A
	// catalog setup failure lands in that row's errors with zero
	// counts and never aborts the pass.
	var rows []SyncInstallResult
	pending := make(map[string][]catalog.Update)
	var order []string
	for _, inst := range installs {
		if !inst.Exists() {
			continue
		}
		row := SyncInstallResult{Root: inst.Root}
		updates, errs := s.checkAllIn(e, inst.AddonsPath)
		if errs != nil {
			row.Errors = errs
		} else {
			pending[inst.AddonsPath] = updates
		}
		order = append(order, inst.AddonsPath)
		rows = append(rows, row)
	}

	// Pass 2: apply each install's collected updates.
	for i, dir := range order {
		row := &rows[i]
		if row.Errors != nil {
			continue // pass-1 failure: nothing to apply
		}
		batch := s.applyUpdates(e, dir, pending[dir], allowReplace)
		row.Updated = batch.AppliedCount
		row.Failed = batch.FailedCount
		if len(batch.errors) > 0 {
			row.Errors = batch.errors
		}
	}

	out := SyncResult{Installs: []SyncInstallResult{}}
	for _, row := range rows {
		out.Installs = append(out.Installs, row)
		out.TotalUpdated += row.Updated
		out.TotalFailed += row.Failed
	}
	return out
}

// wtfRoot returns the WTF directory of an installation: the game root
// plus the flavor subfolder plus WTF.
func wtfRoot(root, flavor string) string {
	return filepath.Join(root, flavor, "WTF")
}

// collectionsDirFor resolves where collection files live: the config
// override, else <config dir>/collections.
func (s *Service) collectionsDirFor(e *env) string {
	if e.cfg.CollectionsDir != "" {
		return e.cfg.CollectionsDir
	}
	return filepath.Join(s.store.Dir(), "collections")
}

// savedVarsManager returns a savedvars.Manager rooted at the install's
// WTF directory.
func (s *Service) savedVarsManager(e *env) *savedvars.Manager {
	return savedvars.New(wtfRoot(e.install.Root, e.install.Flavor), s.log)
}

// pickAccount resolves the account for a savedvars call: the requested
// one, or the first existing one.
func pickAccount(m *savedvars.Manager, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	accts := m.Accounts()
	if len(accts) == 0 {
		return "", fmt.Errorf("no accounts found under %s/Account", m.Root)
	}
	return accts[0], nil
}

// profileIDs returns every supported profile id, for error messages.
func profileIDs() []string {
	ids := make([]string, 0, len(models.Profiles))
	for _, p := range models.Profiles {
		ids = append(ids, p.ID)
	}
	return ids
}

// Source classifier and release-note helpers --------------------------------

var (
	infoWowInterfaceIDRe = regexp.MustCompile(`(?i)info(\d+)(?:-|\.)`)
	infoTukuiIDRe        = regexp.MustCompile(`(?i)/downloads?/([^/?#]+)`)

	mdLinkRe    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdCodeRe    = regexp.MustCompile("`([^`]*)`")
	mdBoldRe    = regexp.MustCompile(`\*\*([^*]*)\*\*`)
	mdItalicRe  = regexp.MustCompile(`\*([^*]*)\*`)
	mdHeadingRe = regexp.MustCompile(`(?m)^\s*#{1,6}\s*`)
)

// classifySource classifies an addon argument into a provider name and
// provider-scoped id, mirroring the catalog's parseSource (which is
// unexported) so `info` needs no catalog API change. It additionally
// accepts a scheme-less "github.com/owner/repo" form.
func classifySource(source string) (string, string, error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return "", "", fmt.Errorf("empty addon argument")
	}
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(lower, "github.com"):
		owner, repo, err := infoGithubPath(s)
		if err != nil {
			return "", "", err
		}
		return catalog.ProviderGitHub, owner + "/" + repo, nil
	case strings.Contains(lower, "curseforge.com"):
		slug, err := infoCurseSlug(s)
		if err != nil {
			return "", "", err
		}
		return catalog.ProviderCurseForge, slug, nil
	case strings.Contains(lower, "wowinterface.com"):
		m := infoWowInterfaceIDRe.FindStringSubmatch(s)
		if m == nil {
			return "", "", fmt.Errorf("cannot parse WowInterface URL %q (expected .../info<id>-<slug>.html)", source)
		}
		return catalog.ProviderWowInterface, m[1], nil
	case strings.Contains(lower, "tukui.org"):
		m := infoTukuiIDRe.FindStringSubmatch(s)
		if m == nil {
			return "", "", fmt.Errorf("cannot parse Tukui URL %q (expected .../downloads/<id>)", source)
		}
		return catalog.ProviderTukui, m[1], nil
	case strings.Contains(s, "/"):
		owner, repo, err := infoGithubPath(s)
		if err != nil {
			return "", "", err
		}
		return catalog.ProviderGitHub, owner + "/" + repo, nil
	default:
		return "", "", fmt.Errorf("unknown addon %q", source)
	}
}

// infoGithubPath extracts owner and repo from "owner/repo" or a
// github.com URL, ignoring any trailing segments.
func infoGithubPath(s string) (owner, repo string, err error) {
	u := s
	if strings.Contains(strings.ToLower(s), "github.com") {
		parsed, err := url.Parse(s)
		if err != nil {
			return "", "", fmt.Errorf("invalid GitHub URL %q: %w", s, err)
		}
		u = parsed.Path
	}
	u = strings.Trim(u, "/")
	parts := strings.Split(u, "/")
	if len(parts) > 1 && (strings.EqualFold(parts[0], "github.com") || strings.EqualFold(parts[0], "www.github.com")) {
		parts = parts[1:]
	}
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("GitHub source %q must be owner/repo", s)
	}
	return parts[0], parts[1], nil
}

// infoCurseSlug extracts the addon slug from a CurseForge URL path
// such as /wow/addons/<slug>.
func infoCurseSlug(s string) (string, error) {
	parsed, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid CurseForge URL %q: %w", s, err)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, p := range parts {
		if strings.EqualFold(p, "addons") && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("cannot parse CurseForge URL %q (expected .../wow/addons/<slug>)", s)
}

// infoReleaseNotes fetches the latest GitHub release notes for the
// addon's repository, stripped of markdown formatting. Any failure
// (network, rate limit, no release) returns an error; callers degrade
// to an empty string instead of failing the call. The service's
// injected http client is used when set so tests can mock GitHub.
func (s *Service) infoReleaseNotes(addon *catalog.Addon) (string, error) {
	owner, repo, ok := strings.Cut(addon.ID, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("github: not an owner/repo id")
	}
	u := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases/latest"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: unexpected status %d", resp.StatusCode)
	}
	var rel struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return stripMarkdown(rel.Body), nil
}

// stripMarkdown reduces release-note markdown to plain text: headings,
// bold/italic emphasis, inline code and links keep their content while
// the syntax is dropped, and blank lines are collapsed.
func stripMarkdown(s string) string {
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = mdCodeRe.ReplaceAllString(s, "$1")
	s = mdBoldRe.ReplaceAllString(s, "$1")
	s = mdItalicRe.ReplaceAllString(s, "$1")
	s = mdHeadingRe.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// AddonInfo resolves one addon from a provider source ("owner/repo" or
// a provider URL) or a bare-name search. A bare name with several
// matches returns them in Matches
// with a nil error so the frontend can disambiguate; exactly one match
// resolves it. ReleaseNotes is filled for GitHub addons only and is
// best-effort (empty when they cannot be fetched).
func (s *Service) AddonInfo(arg string) (InfoResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return InfoResult{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return InfoResult{}, err
	}
	ctx := context.Background()

	var addon *catalog.Addon
	if strings.Contains(arg, "/") {
		providerName, id, err := classifySource(arg)
		if err != nil {
			return InfoResult{}, err
		}
		prov, ok := cat.Provider(providerName)
		if !ok {
			return InfoResult{}, fmt.Errorf("provider %q is not available", providerName)
		}
		addon, err = prov.Resolve(ctx, id)
		if err != nil {
			return InfoResult{}, fmt.Errorf("cannot resolve %q: %w", arg, err)
		}
	} else {
		matches, _ := cat.Search(ctx, arg, 5)
		switch {
		case len(matches) == 0:
			return InfoResult{}, fmt.Errorf("no matches for %q", arg)
		case len(matches) > 1:
			out := InfoResult{Matches: []SearchHit{}}
			for _, m := range matches {
				out.Matches = append(out.Matches, SearchHit{
					Provider:      m.Provider,
					Name:          m.Name,
					Author:        m.Author,
					Summary:       m.Summary,
					LatestVersion: m.LatestVersion,
					GameVersion:   m.GameVersion,
					ID:            m.ID,
					Homepage:      m.Homepage,
				})
			}
			return out, nil
		default:
			addon = matches[0]
		}
	}
	return s.toInfoResult(addon), nil
}

// toInfoResult maps one resolved addon to the detail DTO.
func (s *Service) toInfoResult(addon *catalog.Addon) InfoResult {
	out := InfoResult{
		Provider:      addon.Provider,
		ID:            addon.ID,
		Name:          addon.Name,
		Author:        addon.Author,
		Summary:       addon.Summary,
		LatestVersion: addon.LatestVersion,
		Homepage:      addon.Homepage,
		GameVersion:   addon.GameVersion,
		UpdatedAt:     addon.UpdatedAt,
	}
	if addon.Provider == catalog.ProviderGitHub {
		if notes, err := s.infoReleaseNotes(addon); err == nil && strings.TrimSpace(notes) != "" {
			out.ReleaseNotes = notes
		}
	}
	return out
}

// Sources lists the catalog providers with their caveats. No install
// is required.
func (s *Service) Sources() ([]ProviderInfo, error) {
	return []ProviderInfo{
		{Name: "github", Description: "GitHub releases API — unauthenticated, ~60 requests/hour"},
		{Name: "curseforge", Description: "modern Core API with WOWFIX_CURSEFORGE_API_KEY, else deprecated legacy endpoint"},
		{Name: "wowinterface", Description: "MMOUI filelist JSON"},
		{Name: "tukui", Description: "tukui.org API"},
	}, nil
}

// Doctor runs the environment report, mapped to one DoctorCheck per
// output line. A single failing check
// never aborts the report.
func (s *Service) Doctor() (DoctorReport, error) {
	e, err := s.env()
	if err != nil {
		return DoctorReport{}, err
	}
	var checks []DoctorCheck
	add := func(name, status, message string) {
		checks = append(checks, DoctorCheck{Name: name, Status: status, Message: message})
	}

	add("config", "info", e.store.Path())

	if p := models.ProfileByID(e.cfg.Profile); p == nil {
		add("profile", "error", fmt.Sprintf("unknown profile %q (valid: %s)", e.cfg.Profile, strings.Join(profileIDs(), ", ")))
	} else {
		add("profile", "ok", p.ID)
	}
	switch e.cfg.Theme {
	case "dark", "light":
		add("theme", "ok", e.cfg.Theme)
	default:
		add("theme", "error", fmt.Sprintf("must be dark or light (got %q)", e.cfg.Theme))
	}
	if e.cfg.Collection == "" {
		add("collection", "ok", "(none set)")
	} else if utils.Exists(filepath.Join(s.collectionsDirFor(e), e.cfg.Collection+".json")) {
		add("collection", "ok", e.cfg.Collection)
	} else {
		add("collection", "error", fmt.Sprintf("%q not found in %s", e.cfg.Collection, s.collectionsDirFor(e)))
	}

	if e.install == nil {
		add("install", "error", "none found (use --path or set wow_path in config)")
	} else {
		conf := e.install.Confidence
		if conf == "" {
			conf = "unknown"
		}
		add("install", "ok", e.install.AddonsPath)
		add("flavor", "info", fmt.Sprintf("%q (confidence %s)", e.install.Flavor, conf))
		if e.install.Exe != "" {
			add("exe", "info", fmt.Sprintf("%s (version %s)", e.install.Exe, e.install.Version))
		}
		if err := utils.IsWritable(e.install.AddonsPath); err != nil {
			add("permissions", "error", fmt.Sprintf("AddOns directory is not writable: %v", err))
		} else {
			add("permissions", "ok", "AddOns directory is writable")
		}
		if res, err := s.scan(e); err == nil {
			total, problems, errs := res.Stats()
			add("scan", "info", fmt.Sprintf("%d addon(s): %d problem(s), %d error(s).", total, problems, errs))
		} else {
			add("scan", "warn", fmt.Sprintf("scan failed: %v", err))
		}
	}

	if infos, err := backup.New(s.backupRoot(e), s.log).List(); err == nil {
		add("backups", "info", fmt.Sprintf("%d snapshot(s)", len(infos)))
	} else {
		add("backups", "warn", err.Error())
	}

	trashDir := filepath.Join(e.store.Dir(), "trash")
	if err := utils.EnsureDir(trashDir); err != nil {
		add("trash", "error", err.Error())
	} else if err := utils.IsWritable(trashDir); err != nil {
		add("trash", "error", fmt.Sprintf("not writable (%v)", err))
	} else {
		add("trash", "ok", trashDir+" (writable)")
	}

	regPath := s.registryPath
	if regPath == "" {
		regPath, err = catalog.DefaultPath()
		if err != nil {
			add("registry", "error", err.Error())
			regPath = ""
		}
	}
	if regPath != "" {
		if !utils.Exists(regPath) {
			add("registry", "info", "none (addons installed via catalog will appear here)")
		} else if reg, err := catalog.NewRegistry(regPath); err != nil {
			add("registry", "error", err.Error())
		} else {
			add("registry", "ok", fmt.Sprintf("OK (%d entries)", len(reg.Entries())))
		}
	}

	colsDir := s.collectionsDirFor(e)
	switch {
	case !utils.IsDir(colsDir):
		if e.cfg.CollectionsDir != "" {
			add("collections", "warn", fmt.Sprintf("not configured (%s does not exist)", colsDir))
		} else {
			add("collections", "warn", fmt.Sprintf("not configured (%s)", colsDir))
		}
	default:
		if err := utils.IsWritable(colsDir); err != nil {
			add("collections", "error", fmt.Sprintf("%s (not writable: %v)", colsDir, err))
		} else {
			add("collections", "ok", fmt.Sprintf("%s (writable)", colsDir))
		}
	}

	if e.install == nil {
		add("savedvars", "error", "WTF not found")
	} else {
		wtf := wtfRoot(e.install.Root, e.install.Flavor)
		if !utils.IsDir(wtf) {
			add("savedvars", "error", "WTF not found")
		} else {
			add("savedvars", "info", fmt.Sprintf("%d account(s)", len(savedvars.New(wtf, nil).Accounts())))
		}
	}

	if e.cfg.Theme != "dark" && e.cfg.Theme != "light" {
		add("warning", "warn", "theme must be dark or light")
	}
	return DoctorReport{Checks: checks}, nil
}

// SavedVarsAccounts lists the account directory names under the active
// install's WTF/Account folder.
func (s *Service) SavedVarsAccounts() ([]string, error) {
	e, err := s.requireInstall()
	if err != nil {
		return nil, err
	}
	return s.savedVarsManager(e).Accounts(), nil
}

// SavedVarsList lists one account's SavedVariables files. An empty
// account picks the first existing one.
func (s *Service) SavedVarsList(account string) (SavedVarsListResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return SavedVarsListResult{}, err
	}
	m := s.savedVarsManager(e)
	acct, err := pickAccount(m, account)
	if err != nil {
		return SavedVarsListResult{}, err
	}
	files, err := m.List(acct)
	if err != nil {
		return SavedVarsListResult{}, err
	}
	return SavedVarsListResult{WtfRoot: m.Root, Account: acct, Files: files}, nil
}

// SavedVarsBackup backs up one account's SavedVariables to
// <wtf>/savedvars-backups, the default destination.
func (s *Service) SavedVarsBackup(account string) (SavedVarsBackupResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return SavedVarsBackupResult{}, err
	}
	m := s.savedVarsManager(e)
	acct, err := pickAccount(m, account)
	if err != nil {
		return SavedVarsBackupResult{}, err
	}
	path, err := m.Backup(acct, filepath.Join(m.Root, "savedvars-backups"))
	if err != nil {
		return SavedVarsBackupResult{}, err
	}
	return SavedVarsBackupResult{Path: path, Account: acct}, nil
}

// SavedVarsRestore replaces an account's SavedVariables with the
// contents of a backup path. Destructive by policy: the frontend
// dialog is the confirmation gate, so there is no bool parameter.
func (s *Service) SavedVarsRestore(account, backupPath string) error {
	e, err := s.requireInstall()
	if err != nil {
		return err
	}
	m := s.savedVarsManager(e)
	acct, err := pickAccount(m, account)
	if err != nil {
		return err
	}
	return m.Restore(acct, backupPath)
}

// SavedVarsReset deletes one addon's SavedVariables file, matched by
// exact file stem case-insensitively. Destructive by policy; the
// frontend dialog is the confirmation gate.
func (s *Service) SavedVarsReset(account, addon string) error {
	e, err := s.requireInstall()
	if err != nil {
		return err
	}
	m := s.savedVarsManager(e)
	acct, err := pickAccount(m, account)
	if err != nil {
		return err
	}
	return m.Reset(acct, addon)
}

// SavedVarsMigrate copies SavedVariables from one account to another
// within the same WTF root. addon may be "" to copy every file.
// Existing destination files are never overwritten.
func (s *Service) SavedVarsMigrate(fromAccount, toAccount, addon string) (SavedVarsMigrateResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return SavedVarsMigrateResult{}, err
	}
	copied, err := s.savedVarsManager(e).Migrate(fromAccount, toAccount, addon)
	if err != nil {
		return SavedVarsMigrateResult{}, err
	}
	return SavedVarsMigrateResult{Copied: copied}, nil
}

// BackupNow snapshots every addon folder of the active install,
// mirroring `wowfix backup`.
func (s *Service) BackupNow() (BackupResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return BackupResult{}, err
	}
	id, err := backup.New(s.backupRoot(e), s.log).BackupDir(e.install.AddonsPath, "manual backup")
	if err != nil {
		return BackupResult{}, err
	}
	return BackupResult{ID: id}, nil
}

// ListBackups lists the snapshot history newest-first. No install is
// required (mirrors `wowfix restore` with no argument). The folder
// count is read from each snapshot's manifest, best-effort (0 when the
// manifest is unreadable).
func (s *Service) ListBackups() (ListBackupsResult, error) {
	e, err := s.env()
	if err != nil {
		return ListBackupsResult{}, err
	}
	infos, err := backup.New(s.backupRoot(e), s.log).List()
	if err != nil {
		return ListBackupsResult{}, err
	}
	out := ListBackupsResult{Snapshots: []BackupInfo{}}
	for _, in := range infos {
		out.Snapshots = append(out.Snapshots, BackupInfo{
			ID:        in.ID,
			CreatedAt: in.CreatedAt,
			Reason:    in.Reason,
			Folders:   backupFolderCount(in.Path),
		})
	}
	return out, nil
}

// backupFolderCount returns the manifest entry count of a snapshot
// directory, or 0 when the manifest cannot be read.
func backupFolderCount(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0
	}
	var mf backup.Manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return 0
	}
	return len(mf.Entries)
}

// RestoreBackup restores one snapshot. allowReplace stands in for the
// user's confirmation of replacing existing folders (mirrors
// `wowfix restore <id>` with --yes). No install is required.
func (s *Service) RestoreBackup(id string, allowReplace bool) (RestoreBackupResult, error) {
	e, err := s.env()
	if err != nil {
		return RestoreBackupResult{}, err
	}
	restored, skipped, err := backup.New(s.backupRoot(e), s.log).Restore(id, func(string) bool { return allowReplace })
	if err != nil {
		return RestoreBackupResult{}, err
	}
	return RestoreBackupResult{Restored: restored, Skipped: skipped}, nil
}

// ExportCollection writes the active install's addon set to outPath as
// a JSON manifest, YAML manifest or bundle ZIP, dispatching on the
// extension. An empty collectionID exports the
// current on-disk state; otherwise the named collection's state. ZIP
// exports bundle SavedVariables only when includeSavedVars.
func (s *Service) ExportCollection(outPath, collectionID string, includeSavedVars bool) (ExportResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return ExportResult{}, err
	}
	addons, name, err := s.buildManifestAddons(e, collectionID)
	if err != nil {
		return ExportResult{}, err
	}
	switch strings.ToLower(filepath.Ext(outPath)) {
	case ".json":
		if err := importexport.ExportManifest(name, e.cfg.Profile, addons, outPath); err != nil {
			return ExportResult{}, err
		}
	case ".yaml", ".yml":
		if err := importexport.ExportManifestYAML(name, e.cfg.Profile, addons, outPath); err != nil {
			return ExportResult{}, err
		}
	case ".zip":
		svDir := ""
		if includeSavedVars {
			svDir = s.firstSavedVarsDir(e)
		}
		if err := importexport.ExportZip(name, e.cfg.Profile, addons, e.install.AddonsPath, svDir, outPath); err != nil {
			return ExportResult{}, err
		}
	default:
		return ExportResult{}, fmt.Errorf("export requires a .json or .zip output path")
	}
	return ExportResult{Out: outPath, Addons: len(addons), Collection: e.cfg.Collection}, nil
}

// buildManifestAddons assembles the manifest entries for an export:
// either the named collection's addons or the current on-disk scan
// (skipping .disabled and dot-dirs), enriched with registry source
// information when tracked.
func (s *Service) buildManifestAddons(e *env, collectionID string) ([]importexport.ManifestAddon, string, error) {
	tracked := s.registryEntries(e)
	enrich := func(folder string) importexport.ManifestAddon {
		a := importexport.ManifestAddon{Folder: folder}
		if entry, ok := tracked[strings.ToLower(folder)]; ok && entry.Provider != "" {
			a.Provider = entry.Provider
			a.ID = entry.ID
			a.Source = entry.Source
			a.Version = entry.Version
		}
		return a
	}

	if collectionID != "" {
		m, err := s.profilesFor(e)
		if err != nil {
			return nil, "", err
		}
		c, err := m.Get(collectionID)
		if err != nil {
			return nil, "", err
		}
		addons := make([]importexport.ManifestAddon, 0, len(c.Addons))
		for _, st := range c.Addons {
			addons = append(addons, enrich(st.Folder))
		}
		return addons, c.Name, nil
	}

	entries, err := os.ReadDir(e.install.AddonsPath)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read AddOns directory: %w", err)
	}
	var addons []importexport.ManifestAddon
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		folder := entry.Name()
		if strings.HasSuffix(strings.ToLower(folder), ".disabled") {
			continue
		}
		addons = append(addons, enrich(folder))
	}
	return addons, "wowfix-export", nil
}

// firstSavedVarsDir returns the first account's SavedVariables
// directory, or "" when none exists.
func (s *Service) firstSavedVarsDir(e *env) string {
	wtf := wtfRoot(e.install.Root, e.install.Flavor)
	m := savedvars.New(wtf, s.log)
	accts := m.Accounts()
	if len(accts) == 0 {
		return ""
	}
	dir := filepath.Join(wtf, "Account", accts[0], "SavedVariables")
	if !utils.IsDir(dir) {
		return ""
	}
	return dir
}

// ImportCollection installs addons from a manifest (JSON/YAML), a
// bundle ZIP or a GitHub repo-list URL, dispatching on the argument
// type. Importing is gated by the frontend dialog; the method itself
// never prompts.
func (s *Service) ImportCollection(pathOrURL string) (ImportResult, error) {
	e, err := s.requireInstall()
	if err != nil {
		return ImportResult{}, err
	}
	cat, err := s.catalogFor(e)
	if err != nil {
		return ImportResult{}, err
	}

	var installed []string
	switch {
	case utils.Exists(pathOrURL) && strings.EqualFold(filepath.Ext(pathOrURL), ".zip"):
		installed, err = importexport.ImportZip(pathOrURL, e.install.AddonsPath,
			wtfRoot(e.install.Root, e.install.Flavor), cat, nil)

	case utils.Exists(pathOrURL) && (strings.EqualFold(filepath.Ext(pathOrURL), ".json") ||
		strings.EqualFold(filepath.Ext(pathOrURL), ".yaml") || strings.EqualFold(filepath.Ext(pathOrURL), ".yml")):
		var mf *importexport.Manifest
		mf, err = importexport.ImportManifestAny(pathOrURL)
		if err != nil {
			return ImportResult{}, err
		}
		installed, err = s.installManifest(e, cat, mf)

	case strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://"):
		installed, err = importexport.ImportGitHubList(pathOrURL, e.install.AddonsPath, cat, nil)

	default:
		return ImportResult{}, fmt.Errorf("import requires an existing .json/.yaml/.yml or .zip file, or an http(s) URL")
	}
	if err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Installed: installed}, nil
}

// installManifest installs a parsed manifest: remote entries through
// the catalog, local entries by presence check (a bare manifest has no
// addon payload to copy).
func (s *Service) installManifest(e *env, cat *catalog.Catalog, mf *importexport.Manifest) ([]string, error) {
	var installed []string
	for _, a := range mf.Addons {
		switch {
		case a.Provider != "" || a.Source != "":
			source := a.Source
			if source == "" {
				source = a.ID
			}
			names, err := cat.InstallFromSource(context.Background(), source, e.install.AddonsPath, nil)
			if err != nil {
				return installed, fmt.Errorf("install %q: %w", a.Folder, err)
			}
			installed = append(installed, names...)
		default:
			// Local-only entry: a bare manifest has no payload, so the
			// addon either is already installed or is not part of the
			// import. Presence is only checked.
			_ = utils.IsDir(filepath.Join(e.install.AddonsPath, a.Folder))
		}
	}
	return installed, nil
}

// Config returns the full persisted configuration as a view DTO.
func (s *Service) Config() (ConfigView, error) {
	cfg, err := s.store.Load()
	if err != nil {
		return ConfigView{}, err
	}
	return ConfigView{
		WoWPath:          cfg.WoWPath,
		Flavor:           cfg.Flavor,
		Profile:          cfg.Profile,
		Collection:       cfg.Collection,
		Theme:            cfg.Theme,
		AutoBackup:       cfg.AutoBackup,
		Confirmations:    cfg.Confirmations,
		BackupsDir:       cfg.BackupsDir,
		CurseForgeAPIKey: cfg.CurseForgeAPIKey,
		CollectionsDir:   cfg.CollectionsDir,
	}, nil
}

const configKeysHelp = "keys: wow_path, flavor, profile, theme, autobackup, confirmations, backups_dir, curseforge_api_key, collection, collections_dir"

// SetConfigKey persists one configuration key with the same
// validation as config's setConfigValue, including the "auto_backup"
// alias for "autobackup".
func (s *Service) SetConfigKey(key, value string) error {
	cfg, err := s.store.Load()
	if err != nil {
		return err
	}
	switch key {
	case "wow_path":
		if _, err := detector.DetectPath(value); err != nil {
			return err
		}
		cfg.WoWPath = value
	case "flavor":
		cfg.Flavor = value
	case "profile":
		if models.ProfileByID(value) == nil {
			return fmt.Errorf("unknown profile %q (valid: %s)", value, strings.Join(profileIDs(), ", "))
		}
		cfg.Profile = value
	case "theme":
		if value != "dark" && value != "light" {
			return fmt.Errorf("theme must be dark or light")
		}
		cfg.Theme = value
	case "autobackup", "auto_backup":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("autobackup must be true or false")
		}
		cfg.AutoBackup = b
	case "confirmations":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("confirmations must be true or false")
		}
		cfg.Confirmations = b
	case "backups_dir":
		cfg.BackupsDir = value
	case "curseforge_api_key":
		cfg.CurseForgeAPIKey = value
	case "collection":
		cfg.Collection = value
	case "collections_dir":
		cfg.CollectionsDir = value
	default:
		return fmt.Errorf("unknown key %q\n%s", key, configKeysHelp)
	}
	return s.store.Save(cfg)
}
