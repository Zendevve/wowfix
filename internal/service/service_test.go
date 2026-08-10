// Package service tests the Wails facade end to end against a fake WoW
// tree, entirely under t.TempDir(): no real config, no real game folder.
package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wowfix/wowfix/internal/backup"
	"github.com/wowfix/wowfix/internal/catalog"
	"github.com/wowfix/wowfix/internal/config"
	"github.com/wowfix/wowfix/internal/detector"
	"github.com/wowfix/wowfix/internal/importexport"
	"github.com/wowfix/wowfix/internal/logger"
	"github.com/wowfix/wowfix/internal/models"
	"github.com/wowfix/wowfix/internal/scanner"
)

// writeFixture recreates the testdata/wow fixture layout in a temp
// AddOns directory: the same folders, TOC names and versions as
// internal/e2e's writeFixture.
func writeFixture(t *testing.T, addonsDir string) {
	t.Helper()
	writeTOC := func(relDir, tocName, body string) {
		t.Helper()
		dir := filepath.Join(addonsDir, relDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, tocName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeTOC("AtlasLoot", "AtlasLoot.toc", "## Interface: 30300\n## Title: AtlasLoot\n## Version: 7.0.4\n")
	writeTOC("Questie-main", "Questie.toc", "## Interface: 30300\n## Title: Questie\n## Version: 1.12.2\n")
	writeTOC(filepath.Join("DPSMate-main", "DPSMate"), "DPSMate.toc", "## Interface: 30300\n## Title: DPSMate\n## Version: 1.0\n")
	writeTOC("AuxUI", "Aux-Classic.toc", "## Interface: 30300\n## Title: Aux-Classic\n## Version: 1.0\n")
	writeTOC("Questie", "Questie.toc", "## Interface: 30300\n## Title: Questie\n## Version: 1.12.2\n")

	if err := os.MkdirAll(filepath.Join(addonsDir, "Inventory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(addonsDir, "Inventory", "Inventory.lua"), []byte("local x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(addonsDir, "TempFolder"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeZip creates a zip archive with the given relative paths.
func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// newTestService wires a Service to a fresh config in a temp dir and a
// fake game root with the fixture tree. It returns the service and the
// AddOns path.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	writeFixture(t, addonsDir)

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return New(store), addonsDir
}

func findIssue(addons []Addon, folder, kind string) bool {
	for _, a := range addons {
		if a.FolderName != folder {
			continue
		}
		for _, i := range a.Issues {
			if i.Kind == kind {
				return true
			}
		}
	}
	return false
}

// TestScanFindsProblems scans the fixture and expects every seeded
// problem to surface in the DTO.
func TestScanFindsProblems(t *testing.T) {
	s, _ := newTestService(t)
	res, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if res.Stats.Total < 5 {
		t.Fatalf("stats.total = %d, want >= 5", res.Stats.Total)
	}
	if res.Stats.Problems < 4 {
		t.Fatalf("stats.problems = %d, want >= 4", res.Stats.Problems)
	}

	for _, want := range []struct{ folder, kind string }{
		{"Questie-main", "github-name"},
		{"AuxUI", "toc-mismatch"},
		{"DPSMate", "nested"},
		{"Inventory", "missing-toc"},
		{"TempFolder", "empty"},
	} {
		if !findIssue(res.Addons, want.folder, want.kind) {
			t.Errorf("issue %q on %q not found", want.kind, want.folder)
		}
	}

	// Health score: 100 minus 30 per error issue, 15 per warn, 5 per info.
	// AtlasLoot is clean -> 100; Inventory carries a missing-toc error -> 70.
	for _, a := range res.Addons {
		switch a.FolderName {
		case "AtlasLoot":
			if a.Health != 100 {
				t.Errorf("AtlasLoot health = %d, want 100 (clean addon)", a.Health)
			}
		case "Inventory":
			if a.Health != 70 {
				t.Errorf("Inventory health = %d, want 70 (one error issue)", a.Health)
			}
		}
		if len(a.Issues) > 0 && a.Health >= 100 {
			t.Errorf("addon %q has %d issue(s) but health = %d, want < 100", a.FolderName, len(a.Issues), a.Health)
		}
	}
}

// TestFixAllRepairs fixes the fixture without destructive confirmations
// and expects the safe renames and the flatten to be applied on disk.
func TestFixAllRepairs(t *testing.T) {
	s, addonsDir := newTestService(t)
	if _, err := s.Scan(); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	batch, err := s.FixAll(false)
	if err != nil {
		t.Fatalf("FixAll failed: %v", err)
	}
	if len(batch.Fixes) == 0 {
		t.Fatal("FixAll produced no results")
	}
	for _, f := range batch.Fixes {
		if f.Error != "" {
			t.Errorf("fix %s %s errored: %s", f.Addon, f.Action, f.Error)
		}
	}
	if batch.Fixed < 2 {
		t.Errorf("fixed = %d, want >= 2", batch.Fixed)
	}

	// Safe rename applied: AuxUI -> Aux-Classic.
	for _, name := range []string{"Questie", "Aux-Classic", "DPSMate"} {
		if _, err := os.Stat(filepath.Join(addonsDir, name)); err != nil {
			t.Errorf("addon %q missing after fix: %v", name, err)
		}
	}
	// Nested DPSMate promoted to top level, wrapper gone.
	if _, err := os.Stat(filepath.Join(addonsDir, "DPSMate-main")); !os.IsNotExist(err) {
		t.Errorf("wrapper DPSMate-main should be gone after flatten, stat err = %v", err)
	}
}

// TestValidateTable checks the compatibility table: one row per addon,
// expected interface from the profile, detected values from the TOCs.
func TestValidateTable(t *testing.T) {
	s, _ := newTestService(t)
	vr, err := s.Validate()
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if vr.ProfileID != "wrath" {
		t.Errorf("profile_id = %q, want wrath (default)", vr.ProfileID)
	}
	if vr.Expected != 30300 {
		t.Errorf("expected = %d, want 30300 (wrath)", vr.Expected)
	}
	if len(vr.Addons) != 7 {
		t.Fatalf("rows = %d, want one per fixture addon (7)", len(vr.Addons))
	}

	byName := map[string]Compat{}
	for _, c := range vr.Addons {
		if c.Expected != 30300 {
			t.Errorf("row %q expected = %d, want 30300", c.FolderName, c.Expected)
		}
		byName[c.FolderName] = c
	}
	if c := byName["AtlasLoot"]; c.Detected != 30300 || c.Status != "compatible" {
		t.Errorf("AtlasLoot row = %+v, want detected 30300 / compatible", c)
	}
	if c := byName["Inventory"]; c.TOC != "" || c.Detected != -1 || c.Status != "unknown" {
		t.Errorf("Inventory row = %+v, want empty toc / detected -1 / unknown", c)
	}
}

// TestGetStateStalePath saves a wow_path that does not exist and
// expects GetState to report the setup state (no install, stale path
// kept for the path picker) instead of failing the whole UI.
func TestGetStateStalePath(t *testing.T) {
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	stale := filepath.Join(t.TempDir(), "missing")
	cfg.WoWPath = stale
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s := New(store)

	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState failed for stale wow_path: %v", err)
	}
	if st.HasInstall {
		t.Fatalf("has_install = true, want false for stale path")
	}
	if st.WoWPath != stale {
		t.Fatalf("wow_path = %q, want stale value %q preserved for prefill", st.WoWPath, stale)
	}
	if st.ProfileID != "wrath" {
		t.Fatalf("profile_id = %q, want %q", st.ProfileID, "wrath")
	}
}

// TestGetStateValidPath checks the normal case: a resolvable wow_path
// yields the full install state with no error.
func TestGetStateValidPath(t *testing.T) {
	s, addonsDir := newTestService(t)
	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState failed for valid path: %v", err)
	}
	if !st.HasInstall {
		t.Fatal("has_install = false, want true for valid path")
	}
	if st.AddonsDir != addonsDir {
		t.Errorf("addons_dir = %q, want %q", st.AddonsDir, addonsDir)
	}
	if st.ProfileID != "wrath" {
		t.Errorf("profile_id = %q, want wrath (default)", st.ProfileID)
	}
}

// TestGetStateAutoAdopts covers the first-run adoption path: with no
// saved wow_path, a detected install is adopted (persisted and
// reported as the active install) instead of routing to the setup
// wizard. The autoDetect seam points detection at a temp fixture —
// no real-machine calls.
func TestGetStateAutoAdopts(t *testing.T) {
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Default()); err != nil {
		t.Fatal(err)
	}
	s := New(store)

	root := t.TempDir()
	addonsDir := filepath.Join(root, "_retail_", "Interface", "AddOns")
	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.autoDetect = func(context.Context) ([]detector.Installation, error) {
		// Mirror AutoDetect's per-install DetectPath so the flavor and
		// profile derive from the fixture layout.
		inst, err := detector.DetectPath(root)
		if err != nil {
			return nil, err
		}
		return []detector.Installation{*inst}, nil
	}

	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if !st.HasInstall {
		t.Fatal("has_install = false, want true after auto-adopt")
	}
	if st.WoWPath != root {
		t.Errorf("wow_path = %q, want %q", st.WoWPath, root)
	}
	if st.Flavor != detector.FlavorRetail {
		t.Errorf("flavor = %q, want %q", st.Flavor, detector.FlavorRetail)
	}
	if st.AddonsDir != addonsDir {
		t.Errorf("addons_dir = %q, want %q", st.AddonsDir, addonsDir)
	}
	if st.ProfileID != "retail" {
		t.Errorf("profile_id = %q, want %q", st.ProfileID, "retail")
	}

	// The adoption is persisted: the store file now carries wow_path,
	// flavor and the install-derived profile.
	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.WoWPath != root {
		t.Errorf("saved wow_path = %q, want %q", reloaded.WoWPath, root)
	}
	if reloaded.Flavor != detector.FlavorRetail {
		t.Errorf("saved flavor = %q, want %q", reloaded.Flavor, detector.FlavorRetail)
	}
	if reloaded.Profile != "retail" {
		t.Errorf("saved profile = %q, want %q", reloaded.Profile, "retail")
	}

	// Nothing detected: setup state, no adoption, no persistence.
	s.autoDetect = func(context.Context) ([]detector.Installation, error) {
		return nil, nil
	}
	// Fresh config: forget the adopted path so this exercises the
	// empty-wow_path branch again.
	if err := store.Save(config.Default()); err != nil {
		t.Fatal(err)
	}
	st, err = s.GetState()
	if err != nil {
		t.Fatalf("GetState failed for empty detection: %v", err)
	}
	if st.HasInstall {
		t.Fatal("has_install = true, want false when nothing is detected")
	}
	reloaded, err = store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.WoWPath != "" {
		t.Errorf("saved wow_path = %q, want empty (no adoption)", reloaded.WoWPath)
	}
}

// TestGetStateAutoAdoptSaveFailure covers the degraded adoption path:
// when the config store cannot persist the adopted install, GetState
// logs the failure and still returns the full install state for this
// session instead of bricking first-run boot.
func TestGetStateAutoAdoptSaveFailure(t *testing.T) {
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Default()); err != nil {
		t.Fatal(err)
	}
	s := New(store)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "_retail_", "Interface", "AddOns"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.autoDetect = func(context.Context) ([]detector.Installation, error) {
		inst, err := detector.DetectPath(root)
		if err != nil {
			return nil, err
		}
		return []detector.Installation{*inst}, nil
	}

	// Break Save deterministically on any platform: Save writes the
	// temp sibling before renaming, and a directory squatting on the
	// temp path makes os.WriteFile fail while Load keeps working.
	if err := os.MkdirAll(store.Path()+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState failed on unwritable store: %v", err)
	}
	if !st.HasInstall {
		t.Fatal("has_install = false, want true: adoption must survive a failed persist")
	}
	if st.WoWPath != root {
		t.Errorf("wow_path = %q, want %q", st.WoWPath, root)
	}
	if st.ProfileID != "retail" {
		t.Errorf("profile_id = %q, want %q (in-memory adoption)", st.ProfileID, "retail")
	}

	// The failure is logged, not fatal.
	var logged bool
	for _, e := range s.log.Entries() {
		if e.Level == logger.LevelError && strings.Contains(e.Message, "auto-adopt") {
			logged = true
		}
	}
	if !logged {
		t.Error("no error entry logged for the failed persist")
	}

	// Nothing was persisted: the on-disk config still has no wow_path.
	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.WoWPath != "" {
		t.Errorf("saved wow_path = %q, want empty (persist failed)", reloaded.WoWPath)
	}
}

// TestGetStateAutoAdoptPreservesConfig covers the config-preservation
// contract: adoption changes wow_path/flavor/profile only, leaving
// every other saved preference untouched.
func TestGetStateAutoAdoptPreservesConfig(t *testing.T) {
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.AutoBackup = false
	cfg.Confirmations = false
	cfg.Collection = "some-id"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s := New(store)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "_retail_", "Interface", "AddOns"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.autoDetect = func(context.Context) ([]detector.Installation, error) {
		inst, err := detector.DetectPath(root)
		if err != nil {
			return nil, err
		}
		return []detector.Installation{*inst}, nil
	}

	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if !st.HasInstall {
		t.Fatal("has_install = false, want true after auto-adopt")
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.WoWPath != root {
		t.Errorf("saved wow_path = %q, want %q", reloaded.WoWPath, root)
	}
	if reloaded.Flavor != detector.FlavorRetail {
		t.Errorf("saved flavor = %q, want %q", reloaded.Flavor, detector.FlavorRetail)
	}
	if reloaded.Profile != "retail" {
		t.Errorf("saved profile = %q, want %q", reloaded.Profile, "retail")
	}
	if reloaded.AutoBackup {
		t.Error("saved auto_backup = true, want the seeded false preserved")
	}
	if reloaded.Confirmations {
		t.Error("saved confirmations = true, want the seeded false preserved")
	}
	if reloaded.Collection != "some-id" {
		t.Errorf("saved collection = %q, want %q", reloaded.Collection, "some-id")
	}
}

// TestGetStateAutoAdoptRootFlavor covers a root-layout install:
// Interface/AddOns lives directly under the root (no flavor folder),
// so adoption yields Flavor=="" and still persists.
func TestGetStateAutoAdoptRootFlavor(t *testing.T) {
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Default()); err != nil {
		t.Fatal(err)
	}
	s := New(store)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Interface", "AddOns"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.autoDetect = func(context.Context) ([]detector.Installation, error) {
		inst, err := detector.DetectPath(root)
		if err != nil {
			return nil, err
		}
		return []detector.Installation{*inst}, nil
	}

	st, err := s.GetState()
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if !st.HasInstall {
		t.Fatal("has_install = false, want true after auto-adopt")
	}
	if st.Flavor != "" {
		t.Errorf("flavor = %q, want empty (root layout)", st.Flavor)
	}
	if st.WoWPath != root {
		t.Errorf("wow_path = %q, want %q", st.WoWPath, root)
	}
	// Root layout has no install-derived profile: the saved default
	// stays untouched.
	if st.ProfileID != "wrath" {
		t.Errorf("profile_id = %q, want %q (saved default)", st.ProfileID, "wrath")
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.WoWPath != root {
		t.Errorf("saved wow_path = %q, want %q", reloaded.WoWPath, root)
	}
	if reloaded.Flavor != "" {
		t.Errorf("saved flavor = %q, want empty", reloaded.Flavor)
	}
	if reloaded.Profile != "wrath" {
		t.Errorf("saved profile = %q, want %q (untouched)", reloaded.Profile, "wrath")
	}
}

// TestSetInstallPersistsProfile covers the fallback setup flow: a
// detected profile is persisted alongside wow_path and flavor, so the
// install carries its game version without a separate SetProfile call.
func TestSetInstallPersistsProfile(t *testing.T) {
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Default()); err != nil {
		t.Fatal(err)
	}
	s := New(store)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "_retail_", "Interface", "AddOns"), 0o755); err != nil {
		t.Fatal(err)
	}

	inst, err := s.SetInstall(root, "")
	if err != nil {
		t.Fatalf("SetInstall failed: %v", err)
	}
	if inst.Flavor != detector.FlavorRetail {
		t.Errorf("flavor = %q, want %q", inst.Flavor, detector.FlavorRetail)
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.WoWPath != root {
		t.Errorf("saved wow_path = %q, want %q", reloaded.WoWPath, root)
	}
	if reloaded.Flavor != detector.FlavorRetail {
		t.Errorf("saved flavor = %q, want %q", reloaded.Flavor, detector.FlavorRetail)
	}
	if reloaded.Profile != "retail" {
		t.Errorf("saved profile = %q, want %q", reloaded.Profile, "retail")
	}
}

// TestInstallZip installs an archive built in memory and checks the
// folder lands on disk and is reported as installed.
func TestInstallZip(t *testing.T) {
	s, addonsDir := newTestService(t)
	zipPath := filepath.Join(t.TempDir(), "newaddon.zip")
	writeZip(t, zipPath, map[string]string{
		"NewAddon/NewAddon.toc": "## Interface: 30300\n## Title: NewAddon\n## Version: 1.0.0\n",
	})

	res, err := s.InstallZip(zipPath, true)
	if err != nil {
		t.Fatalf("InstallZip failed: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("install errors: %v", res.Errors)
	}
	if !slices.Contains(res.Installed, "NewAddon") {
		t.Errorf("installed = %v, want NewAddon", res.Installed)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "NewAddon")); err != nil {
		t.Errorf("NewAddon folder missing after install: %v", err)
	}
}

// rewriteTransport redirects api.github.com and codeload.github.com
// traffic to a mock server so the real GitHub provider never touches
// the network.
type rewriteTransport struct {
	mock string // mock origin "host:port"
	base http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Host {
	case "api.github.com", "codeload.github.com", "github.com":
	default:
		return nil, fmt.Errorf("test transport refuses non-GitHub host %s", req.URL.Host)
	}
	r := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = t.mock
	r.URL = &u
	return t.base.RoundTrip(r)
}

// mockGitHub serves the GitHub endpoints the real provider hits:
// repository metadata, latest releases, release zip assets and
// per-tag release lookups (rollback).
type mockGitHub struct {
	repos   map[string]string // "owner/repo" -> latest release tag
	zips    map[string][]byte // "owner/repo" -> archive bytes for Download
	results []string          // "owner/repo" names returned by search
	notes   map[string]string // "owner/repo" -> latest release notes body
	tags    map[string]bool   // "owner/repo/tag" -> resolvable past release
}

// client returns an http.Client whose GitHub traffic reaches only the
// mock.
func (m *mockGitHub) client(t *testing.T) *http.Client {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/repositories":
			// The provider first tries a topic-qualified query; reject
			// it so the plain query fallback is exercised.
			if strings.Contains(r.URL.Query().Get("q"), "topic:") {
				http.Error(w, "422 Unprocessable Entity", http.StatusUnprocessableEntity)
				return
			}
			q := strings.ToLower(r.URL.Query().Get("q"))
			var items []string
			for _, full := range m.results {
				owner, name, ok := strings.Cut(full, "/")
				if !ok || (q != "" && !strings.Contains(strings.ToLower(name), q)) {
					continue
				}
				items = append(items, fmt.Sprintf(
					`{"full_name":%q,"name":%q,"description":"","html_url":"https://github.com/%s","default_branch":"main","owner":{"login":%q}}`,
					full, name, full, owner))
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
		case strings.HasPrefix(r.URL.EscapedPath(), "/dl/"):
			id := strings.ReplaceAll(strings.TrimPrefix(r.URL.EscapedPath(), "/dl/"), "%2F", "/")
			data, ok := m.zips[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			rest := strings.TrimPrefix(r.URL.Path, "/repos/")
			if id, tag, ok := strings.Cut(rest, "/releases/tags/"); ok {
				if !m.tags[id+"/"+tag] {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"addon.zip","browser_download_url":"https://github.com/dl/%s"}]}`,
					tag, url.PathEscape(id))
				return
			}
			if id, ok := strings.CutSuffix(rest, "/releases/latest"); ok {
				tag, ok := m.repos[id]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"addon.zip","browser_download_url":"https://github.com/dl/%s"}],"body":%q}`,
					tag, url.PathEscape(id), m.notes[id])
				return
			}
			owner, name, ok := strings.Cut(rest, "/")
			if !ok {
				http.NotFound(w, r)
				return
			}
			if _, known := m.repos[rest]; !known {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"full_name":%q,"name":%q,"description":"","html_url":"https://github.com/%s","default_branch":"main","owner":{"login":%q}}`,
				rest, name, rest, owner)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return &http.Client{
		Transport: &rewriteTransport{mock: strings.TrimPrefix(ts.URL, "http://"), base: ts.Client().Transport},
	}
}

// addonZipBytes builds an addon archive in memory: one folder with a
// single TOC file.
func addonZipBytes(t *testing.T, folder, toc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(folder + "/" + folder + ".toc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(toc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newTestCatalogService wires a Service to a fake install, an
// isolated registry and a github-only catalog whose traffic goes to a
// mock GitHub server. It returns the service, the AddOns path, the
// registry path and the mock.
func newTestCatalogService(t *testing.T) (*Service, string, string, *mockGitHub) {
	t.Helper()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	writeFixture(t, addonsDir)

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s := New(store)
	s.registryPath = filepath.Join(t.TempDir(), "registry.json")
	s.enabledProviders = map[string]bool{catalog.ProviderGitHub: true}
	mock := &mockGitHub{repos: map[string]string{}, zips: map[string][]byte{}, notes: map[string]string{}}
	s.httpClient = mock.client(t)
	return s, addonsDir, s.registryPath, mock
}

// TestSearchCatalog checks the search wiring: mock GitHub results
// arrive in the DTO shape.
func TestSearchCatalog(t *testing.T) {
	s, _, _, mock := newTestCatalogService(t)
	mock.results = []string{"xperl/xperl", "flux/flux"}

	res, err := s.SearchCatalog("x")
	if err != nil {
		t.Fatalf("SearchCatalog: %v", err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(res.Results), res.Results)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %v, want none", res.Errors)
	}
	byName := map[string]SearchHit{}
	for _, h := range res.Results {
		byName[h.Name] = h
	}
	h, ok := byName["xperl"]
	if !ok {
		t.Fatalf("xperl missing from results: %+v", res.Results)
	}
	if h.Provider != "github" || h.ID != "xperl/xperl" || h.Author != "xperl" ||
		h.Homepage != "https://github.com/xperl/xperl" {
		t.Errorf("xperl row = %+v", h)
	}
}

// TestInstallSource installs a real archive through the github
// provider's Download and checks the folder lands on disk and is
// tracked in the registry.
func TestInstallSource(t *testing.T) {
	s, addonsDir, regPath, mock := newTestCatalogService(t)
	mock.repos["acme/newaddon"] = "v1.0.0"
	mock.zips["acme/newaddon"] = addonZipBytes(t, "NewAddon", "## Title: NewAddon\n## Version: 1.0.0\n## Interface: 30300\n")

	res, err := s.InstallSource("acme/newaddon", true)
	if err != nil {
		t.Fatalf("InstallSource: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %v, want none", res.Errors)
	}
	if !slices.Contains(res.Installed, "NewAddon") {
		t.Errorf("installed = %v, want NewAddon", res.Installed)
	}
	if len(res.Replaced) != 0 || len(res.Skipped) != 0 {
		t.Errorf("replaced/skipped should stay empty, got %v/%v", res.Replaced, res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "NewAddon", "NewAddon.toc")); err != nil {
		t.Fatalf("installed TOC missing: %v", err)
	}

	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := reg.Entries()
	if len(entries) != 1 {
		t.Fatalf("registry entries = %d, want 1: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Folder != "NewAddon" || e.Provider != "github" || e.ID != "acme/newaddon" {
		t.Errorf("entry = %+v", e)
	}
	if e.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0 (read from the installed TOC)", e.Version)
	}
	if e.Source != "acme/newaddon" {
		t.Errorf("source = %q, want acme/newaddon", e.Source)
	}
}

// TestCheckUpdates reports an update whose latest version bumps the
// tracked one.
func TestCheckUpdates(t *testing.T) {
	s, _, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Track(catalog.Entry{
		Folder: "Alpha", Title: "Alpha", Version: "1.0.0",
		Provider: "github", ID: "acme/alpha", Source: "acme/alpha",
	}); err != nil {
		t.Fatal(err)
	}
	mock.repos["acme/alpha"] = "v2.0.0"

	res, err := s.CheckUpdates()
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %v, want none", res.Errors)
	}
	if len(res.Updates) != 1 {
		t.Fatalf("updates = %d, want 1: %+v", len(res.Updates), res.Updates)
	}
	u := res.Updates[0]
	if u.Folder != "Alpha" || u.CurrentVersion != "1.0.0" || u.LatestVersion != "v2.0.0" ||
		u.Provider != "github" || u.ID != "acme/alpha" || u.Source != "acme/alpha" {
		t.Errorf("update = %+v", u)
	}
	if u.FlavorMismatch {
		t.Errorf("flavor_mismatch = true, want false (no game version in repo metadata)")
	}
	if _, err := time.Parse(time.RFC3339, res.CheckedAt); err != nil {
		t.Errorf("checked_at %q is not RFC3339: %v", res.CheckedAt, err)
	}
}

// TestCheckUpdatesPartialFailure keeps healthy updates when another
// entry's provider lookup fails.
func TestCheckUpdatesPartialFailure(t *testing.T) {
	s, _, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Alpha", Title: "Alpha", Version: "1.0.0", Provider: "github", ID: "acme/alpha"})
	_ = reg.Track(catalog.Entry{Folder: "Broken", Title: "Broken", Version: "1.0.0", Provider: "github", ID: "acme/broken"})
	mock.repos["acme/alpha"] = "v2.0.0"
	// acme/broken has no repository metadata: the lookup fails.

	res, err := s.CheckUpdates()
	if err != nil {
		t.Fatalf("CheckUpdates: %v", err)
	}
	if len(res.Updates) != 1 || res.Updates[0].Folder != "Alpha" {
		t.Fatalf("updates = %+v, want only Alpha", res.Updates)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want the Broken lookup failure", res.Errors)
	}
}

// TestApplyUpdate applies one pending update and checks the folder
// and registry are refreshed.
func TestApplyUpdate(t *testing.T) {
	s, addonsDir, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Track(catalog.Entry{
		Folder: "Questie", Title: "Questie", Version: "9.0.0",
		Provider: "github", ID: "acme/questie", Source: "acme/questie",
	}); err != nil {
		t.Fatal(err)
	}
	mock.repos["acme/questie"] = "v9.2.0"
	mock.zips["acme/questie"] = addonZipBytes(t, "Questie", "## Title: Questie\n## Version: 9.2.0\n## Interface: 30300\n")

	batch, err := s.ApplyUpdate("Questie", true)
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if batch.AppliedCount != 1 || batch.FailedCount != 0 || len(batch.Applied) != 1 {
		t.Fatalf("batch = %+v, want 1 applied / 0 failed", batch)
	}
	if a := batch.Applied[0]; !a.OK || a.Folder != "Questie" || a.Error != "" {
		t.Errorf("applied entry = %+v", batch.Applied[0])
	}
	toc, err := os.ReadFile(filepath.Join(addonsDir, "Questie", "Questie.toc"))
	if err != nil {
		t.Fatalf("read updated TOC: %v", err)
	}
	if !strings.Contains(string(toc), "## Version: 9.2.0") {
		t.Errorf("Questie TOC not updated: %s", toc)
	}
	reg, err = catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if entries := reg.Entries(); len(entries) != 1 || entries[0].Version != "9.2.0" {
		t.Errorf("registry after update = %+v", entries)
	}
}

// TestApplyUpdateDeclinesReplace skips the update when allowReplace
// is false and the folder exists.
func TestApplyUpdateDeclinesReplace(t *testing.T) {
	s, _, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{
		Folder: "Questie", Title: "Questie", Version: "9.0.0",
		Provider: "github", ID: "acme/questie", Source: "acme/questie",
	})
	mock.repos["acme/questie"] = "v9.2.0"

	batch, err := s.ApplyUpdate("Questie", false)
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if batch.FailedCount != 1 || len(batch.Applied) != 1 {
		t.Fatalf("batch = %+v, want 1 failed", batch)
	}
	a := batch.Applied[0]
	if a.OK {
		t.Error("entry should not be applied")
	}
	if a.Message != "folder already exists, replace declined" {
		t.Errorf("message = %q, want the replace-declined message", a.Message)
	}
}

// TestApplyUpdateNotFound reports a failed entry with a clear message
// when no update matches the folder.
func TestApplyUpdateNotFound(t *testing.T) {
	s, _, _, _ := newTestCatalogService(t)
	batch, err := s.ApplyUpdate("Missing", true)
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if batch.FailedCount != 1 || len(batch.Applied) != 1 {
		t.Fatalf("batch = %+v, want 1 failed", batch)
	}
	if batch.Applied[0].OK {
		t.Error("entry should not be applied")
	}
	if !strings.Contains(batch.Applied[0].Message, "Missing") {
		t.Errorf("message = %q, want it to name the folder", batch.Applied[0].Message)
	}
}

// TestScanReportsTrackedDrifted seeds the registry with a checksummed
// entry for a fixture addon and checks the scan DTO reports it
// tracked and clean, then drifted once a file changes. Entries without
// a checksum baseline (pre-integrity installs) stay tracked but never
// drift.
func TestScanReportsTrackedDrifted(t *testing.T) {
	s, addonsDir, regPath, _ := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := catalog.ComputeManifest(filepath.Join(addonsDir, "AtlasLoot"))
	if err != nil {
		t.Fatalf("ComputeManifest: %v", err)
	}
	if err := reg.Track(catalog.Entry{
		Folder: "AtlasLoot", Title: "AtlasLoot", Version: "7.0.4",
		Provider: "github", ID: "acme/atlasloot", Source: "acme/atlasloot",
		Checksum: sum,
	}); err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{
		Folder: "Questie", Title: "Questie", Version: "1.12.2",
		Provider: "github", ID: "acme/questie", Source: "acme/questie",
	})

	byFolder := func() map[string]Addon {
		t.Helper()
		res, err := s.Scan()
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out := map[string]Addon{}
		for _, a := range res.Addons {
			out[a.FolderName] = a
		}
		return out
	}

	clean := byFolder()
	atlas := clean["AtlasLoot"]
	if !atlas.Tracked || atlas.Drifted || atlas.TrackedSource != "acme/atlasloot" {
		t.Errorf("AtlasLoot = %+v, want tracked / not drifted / source acme/atlasloot", atlas)
	}
	questie := clean["Questie"]
	if !questie.Tracked || questie.Drifted {
		t.Errorf("Questie = %+v, want tracked without a checksum baseline, never drifted", questie)
	}
	aux := clean["AuxUI"]
	if aux.Tracked || aux.Drifted || aux.TrackedSource != "" {
		t.Errorf("AuxUI = %+v, want untracked", aux)
	}

	// Touching a file inside the tracked folder flips Drifted on the
	// next scan; the checksum-less entry is unaffected.
	toc := filepath.Join(addonsDir, "AtlasLoot", "AtlasLoot.toc")
	f, err := os.OpenFile(toc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n-- tampered\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dirty := byFolder()
	if a := dirty["AtlasLoot"]; !a.Tracked || !a.Drifted {
		t.Errorf("AtlasLoot after edit = %+v, want tracked and drifted", a)
	}
	if a := dirty["Questie"]; a.Drifted {
		t.Errorf("Questie without checksum baseline = %+v, want not drifted", a)
	}
}

// TestRestoreAddon restores a tracked addon from its recorded source
// through the mock GitHub provider and checks the folder and registry
// are refreshed; an untracked folder lands in the DTO errors with a
// nil Go error.
func TestRestoreAddon(t *testing.T) {
	s, addonsDir, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Track(catalog.Entry{
		Folder: "Questie", Title: "Questie", Version: "1.12.2",
		Provider: "github", ID: "acme/questie", Source: "acme/questie",
	}); err != nil {
		t.Fatal(err)
	}
	mock.repos["acme/questie"] = "v1.13.0"
	mock.zips["acme/questie"] = addonZipBytes(t, "Questie", "## Title: Questie\n## Version: 1.13.0\n## Interface: 30300\n")

	// Untracked folder: reported as an error in the DTO, nil Go error.
	missing, err := s.RestoreAddon("AtlasLoot", true)
	if err != nil {
		t.Fatalf("RestoreAddon(untracked): %v", err)
	}
	if !slices.Contains(missing.Errors, "addon not tracked in registry") {
		t.Errorf("errors = %v, want the not-tracked message", missing.Errors)
	}

	res, err := s.RestoreAddon("Questie", true)
	if err != nil {
		t.Fatalf("RestoreAddon: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %v, want none", res.Errors)
	}
	if !slices.Contains(res.Installed, "Questie") {
		t.Errorf("installed = %v, want Questie", res.Installed)
	}
	toc, err := os.ReadFile(filepath.Join(addonsDir, "Questie", "Questie.toc"))
	if err != nil {
		t.Fatalf("read restored TOC: %v", err)
	}
	if !strings.Contains(string(toc), "## Version: 1.13.0") {
		t.Errorf("Questie TOC not restored: %s", toc)
	}

	// The catalog re-records the manifest checksum after the restore.
	reg, err = catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := reg.Entries()
	if len(entries) != 1 {
		t.Fatalf("registry entries = %d, want 1: %+v", len(entries), entries)
	}
	got, err := catalog.ComputeManifest(filepath.Join(addonsDir, "Questie"))
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Checksum != got {
		t.Errorf("recorded checksum %q != computed %q", entries[0].Checksum, got)
	}
}

// collectionTestService wires a Service to a temp config with an
// install whose AddOns dir holds the given folder names, plus a temp
// collections dir via cfg.CollectionsDir.
func collectionTestService(t *testing.T, folders ...string) (*Service, string) {
	t.Helper()
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	for _, name := range folders {
		if err := os.MkdirAll(filepath.Join(addonsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg.WoWPath = root
	cfg.Flavor = ""
	cfg.CollectionsDir = filepath.Join(t.TempDir(), "collections")
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return New(store), addonsDir
}

// TestCollectionsLifecycle walks the collection CRUD surface: create
// snapshots the on-disk folders (enabled and .disabled both counted),
// list reports one inactive collection, SetCollectionAddon /
// CollectionDetail round-trip the toggle, and delete empties the list.
func TestCollectionsLifecycle(t *testing.T) {
	s, _ := collectionTestService(t, "A", "A.disabled")

	created, err := s.CreateCollection("pve")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if created.Active {
		t.Error("created collection must not be active")
	}
	if created.AddonCount != 2 {
		t.Errorf("addon_count = %d, want 2 (A enabled + A.disabled)", created.AddonCount)
	}

	res, err := s.Collections()
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if res.ActiveID != "" {
		t.Errorf("active_id = %q, want empty (create must not activate)", res.ActiveID)
	}
	if len(res.Collections) != 1 {
		t.Fatalf("collections = %d, want 1", len(res.Collections))
	}
	c := res.Collections[0]
	if c.ID != created.ID || c.Name != "pve" || c.Active || c.AddonCount != 2 {
		t.Errorf("collection = %+v, want id/name %q/%q, active=false, addon_count=2",
			c, created.ID, "pve")
	}

	// Toggle the shared folder off: every recorded entry turns disabled.
	if err := s.SetCollectionAddon(created.ID, "A", false); err != nil {
		t.Fatalf("SetCollectionAddon(false): %v", err)
	}
	detail, err := s.CollectionDetail(created.ID)
	if err != nil {
		t.Fatalf("CollectionDetail: %v", err)
	}
	if detail.ID != created.ID || detail.Name != "pve" || len(detail.Addons) != 2 {
		t.Fatalf("detail = %+v, want 2 addon rows", detail)
	}
	for _, a := range detail.Addons {
		if a.Folder != "A" {
			t.Errorf("addon folder = %q, want A", a.Folder)
		}
		if a.Enabled {
			t.Errorf("addon %q still enabled after SetCollectionAddon(false)", a.Folder)
		}
	}

	// Toggle back on: at least the first entry flips to enabled.
	if err := s.SetCollectionAddon(created.ID, "A", true); err != nil {
		t.Fatalf("SetCollectionAddon(true): %v", err)
	}
	detail, err = s.CollectionDetail(created.ID)
	if err != nil {
		t.Fatalf("CollectionDetail after toggle: %v", err)
	}
	enabled := 0
	for _, a := range detail.Addons {
		if a.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		t.Error("no enabled addons after SetCollectionAddon(true)")
	}

	if err := s.DeleteCollection(created.ID); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}
	after, err := s.Collections()
	if err != nil {
		t.Fatalf("Collections after delete: %v", err)
	}
	if len(after.Collections) != 0 {
		t.Errorf("collections after delete = %d, want 0", len(after.Collections))
	}
}

// TestSwitchCollection activates a collection and verifies the folder
// renames on disk, the applied list, and that the active collection id
// is persisted in the saved config. The second switch exercises the
// reverse rename (A.disabled -> A) with a collection that wants the
// addon enabled.
func TestSwitchCollection(t *testing.T) {
	s, addonsDir := collectionTestService(t, "A")

	created, err := s.CreateCollection("pve")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// On-disk state is A (enabled); record the addon as disabled so
	// the switch renames A -> A.disabled.
	if err := s.SetCollectionAddon(created.ID, "A", false); err != nil {
		t.Fatalf("SetCollectionAddon: %v", err)
	}

	res, err := s.SwitchCollection(created.ID)
	if err != nil {
		t.Fatalf("SwitchCollection: %v", err)
	}
	if !slices.Contains(res.Applied, "A") {
		t.Errorf("applied = %v, want A", res.Applied)
	}
	if res.Message == "" {
		t.Error("message must not be empty")
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "A")); !os.IsNotExist(err) {
		t.Errorf("folder A still present after switch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "A.disabled")); err != nil {
		t.Errorf("folder A.disabled missing after switch: %v", err)
	}

	// The active collection id is persisted in the saved config.
	loaded, err := s.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Collection != created.ID {
		t.Errorf("cfg.collection = %q, want %q", loaded.Collection, created.ID)
	}

	// Now the disk holds A.disabled. A second collection that wants the
	// addon enabled must rename it back.
	back, err := s.CreateCollection("pve-alt")
	if err != nil {
		t.Fatalf("CreateCollection(pve-alt): %v", err)
	}
	if err := s.SetCollectionAddon(back.ID, "A", true); err != nil {
		t.Fatalf("SetCollectionAddon(back): %v", err)
	}
	res, err = s.SwitchCollection(back.ID)
	if err != nil {
		t.Fatalf("second SwitchCollection: %v", err)
	}
	if !slices.Contains(res.Applied, "A") {
		t.Errorf("second applied = %v, want A", res.Applied)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "A")); err != nil {
		t.Errorf("folder A missing after switch back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "A.disabled")); !os.IsNotExist(err) {
		t.Errorf("folder A.disabled still present after switch back: %v", err)
	}
}

// TestInstallsStatus checks the per-install DTO mapping through the
// private seam (AutoDetect cannot be pointed at temp dirs): a
// hand-built installation over the fixture AddOns dir yields the same
// counts as a direct scan and the average addon health, and a missing
// AddOns dir stays exists=false with zeroed counts.
func TestInstallsStatus(t *testing.T) {
	s, addonsDir := newTestService(t)
	root := filepath.Dir(filepath.Dir(addonsDir))
	profile := models.DefaultProfile()

	inst := &detector.Installation{
		Root:       root,
		Flavor:     "",
		AddonsPath: addonsDir,
		Exe:        "Wow.exe",
		Version:    "3.4.3",
		ProfileID:  "wrath",
		Confidence: "high",
	}
	st := s.statusForInstall(inst, profile)
	if !st.Exists {
		t.Fatal("exists = false, want true for the fixture addons dir")
	}
	if st.Exe != "Wow.exe" || st.Version != "3.4.3" || st.ProfileID != "wrath" || st.Confidence != "high" {
		t.Errorf("identity fields not copied: %+v", st)
	}

	// Counts must match an independently run scan of the same dir.
	direct, err := scanner.New(addonsDir, profile).Scan(context.Background())
	if err != nil {
		t.Fatalf("direct scan: %v", err)
	}
	wantTotal, wantProblems, wantErrors := direct.Stats()
	if st.Addons != wantTotal || st.Problems != wantProblems || st.Errors != wantErrors {
		t.Errorf("counts = %d/%d/%d, want scan stats %d/%d/%d",
			st.Addons, st.Problems, st.Errors, wantTotal, wantProblems, wantErrors)
	}
	if st.Errors == 0 {
		t.Error("errors = 0, want the fixture's missing-TOC errors")
	}
	wantHealth := 0
	for _, a := range direct.Addons {
		wantHealth += addonHealth(a)
	}
	wantHealth /= wantTotal
	if st.Health != wantHealth {
		t.Errorf("health = %d, want %d (average addonHealth over the scan)", st.Health, wantHealth)
	}

	// Missing AddOns dir: exists false, zeroed counts, zero health.
	missing := &detector.Installation{Root: root, AddonsPath: filepath.Join(root, "Interface", "Missing")}
	st2 := s.statusForInstall(missing, profile)
	if st2.Exists {
		t.Error("exists = true, want false for a missing addons dir")
	}
	if st2.Addons != 0 || st2.Problems != 0 || st2.Errors != 0 || st2.Health != 0 {
		t.Errorf("missing install counts = %+v, want all zero", st2)
	}
}

// TestSyncUpdatesToAll iterates every install with an existing AddOns
// dir, applies the pending update to each, and aggregates the totals;
// an install without an AddOns dir is skipped.
func TestSyncUpdatesToAll(t *testing.T) {
	s, addonsDir, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Track(catalog.Entry{
		Folder: "Questie", Title: "Questie", Version: "9.0.0",
		Provider: "github", ID: "acme/questie", Source: "acme/questie",
	}); err != nil {
		t.Fatal(err)
	}
	mock.repos["acme/questie"] = "v9.2.0"
	mock.zips["acme/questie"] = addonZipBytes(t, "Questie", "## Title: Questie\n## Version: 9.2.0\n## Interface: 30300\n")

	e, err := s.requireInstall()
	if err != nil {
		t.Fatalf("requireInstall: %v", err)
	}

	// Second install: an existing but empty AddOns dir. Third: no
	// AddOns dir at all (must be skipped).
	other := filepath.Join(t.TempDir(), "Interface", "AddOns")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(addonsDir))
	otherRoot := filepath.Dir(filepath.Dir(other))
	installs := []detector.Installation{
		{Root: root, Flavor: "", AddonsPath: addonsDir, ProfileID: "wrath", Confidence: "high"},
		{Root: otherRoot, Flavor: "", AddonsPath: other, ProfileID: "wrath", Confidence: "medium"},
		{Root: otherRoot, Flavor: "", AddonsPath: filepath.Join(other, "missing"), ProfileID: "wrath", Confidence: "medium"},
	}

	res := s.syncInstalls(e, installs, true)
	if len(res.Installs) != 2 {
		t.Fatalf("rows = %d, want 2 (missing install skipped): %+v", len(res.Installs), res.Installs)
	}
	// Both installs were checked against the same pre-apply registry
	// baseline (two-pass: check all, then apply all), so the tracked
	// addon is updated in each of them.
	if res.TotalUpdated != 2 || res.TotalFailed != 0 {
		t.Errorf("totals = %d updated / %d failed, want 2 / 0", res.TotalUpdated, res.TotalFailed)
	}
	for i, row := range res.Installs {
		if row.Updated != 1 || row.Failed != 0 || len(row.Errors) != 0 {
			t.Errorf("row %d = %+v, want 1 updated / 0 failed / no errors", i, row)
		}
	}

	// Both install directories received the updated TOC.
	for _, dir := range []string{addonsDir, other} {
		toc, err := os.ReadFile(filepath.Join(dir, "Questie", "Questie.toc"))
		if err != nil {
			t.Fatalf("read updated TOC in %s: %v", dir, err)
		}
		if !strings.Contains(string(toc), "## Version: 9.2.0") {
			t.Errorf("Questie TOC not updated in %s: %s", dir, toc)
		}
	}
}

// TestSyncUpdatesToAllCatalogError keeps processing every install when
// one's catalog cannot be resolved: the failure lands in the row's
// errors with zero counts and never aborts the loop.
func TestSyncUpdatesToAllCatalogError(t *testing.T) {
	s, addonsDir, regPath, _ := newTestCatalogService(t)
	// A corrupt registry makes catalogFor fail for every install.
	if err := os.WriteFile(regPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := s.requireInstall()
	if err != nil {
		t.Fatalf("requireInstall: %v", err)
	}
	root := filepath.Dir(filepath.Dir(addonsDir))
	installs := []detector.Installation{
		{Root: root, Flavor: "", AddonsPath: addonsDir, ProfileID: "wrath", Confidence: "high"},
		{Root: root, Flavor: "", AddonsPath: addonsDir, ProfileID: "wrath", Confidence: "high"},
	}

	res := s.syncInstalls(e, installs, true)
	if len(res.Installs) != 2 {
		t.Fatalf("rows = %d, want 2 (loop must not abort on the first failure)", len(res.Installs))
	}
	for _, row := range res.Installs {
		if row.Updated != 0 || row.Failed != 0 {
			t.Errorf("row = %+v, want 0 updated / 0 failed on a catalog error", row)
		}
		if len(row.Errors) != 1 || !strings.Contains(row.Errors[0], "corrupt") {
			t.Errorf("row errors = %v, want the corrupt-registry message", row.Errors)
		}
	}
	if res.TotalUpdated != 0 || res.TotalFailed != 0 {
		t.Errorf("totals = %d updated / %d failed, want 0 / 0", res.TotalUpdated, res.TotalFailed)
	}
}

// newTrackedTestService wires a Service to a fake install, an isolated
// registry, and an explicit backups directory (so the backup root does
// not depend on the detector). It returns the service, the AddOns
// path, the registry path and the backups directory.
func newTrackedTestService(t *testing.T) (*Service, string, string, string) {
	t.Helper()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	writeFixture(t, addonsDir)

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	backupsDir := filepath.Join(t.TempDir(), "backups")
	cfg.BackupsDir = backupsDir
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s := New(store)
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	s.registryPath = registryPath
	return s, addonsDir, registryPath, backupsDir
}

func TestTrackedAddons(t *testing.T) {
	s, _, registryPath, _ := newTrackedTestService(t)

	// Empty registry: empty list, nil error.
	res, err := s.TrackedAddons()
	if err != nil {
		t.Fatalf("TrackedAddons on empty registry: %v", err)
	}
	if len(res.Addons) != 0 {
		t.Fatalf("addons = %d, want 0", len(res.Addons))
	}

	reg, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie-main", Title: "Questie", Version: "1.12.2", Provider: "github", ID: "Questie/Questie", Source: "Questie/Questie", Pinned: true})
	_ = reg.Track(catalog.Entry{Folder: "AtlasLoot", Title: "AtlasLoot", Version: "7.0.4", Provider: "curseforge", ID: "atlasloot", Source: "https://www.curseforge.com/wow/addons/atlasloot", Ignored: true})

	res, err = s.TrackedAddons()
	if err != nil {
		t.Fatalf("TrackedAddons: %v", err)
	}
	if len(res.Addons) != 2 {
		t.Fatalf("addons = %d, want 2: %+v", len(res.Addons), res.Addons)
	}
	// Sorted by folder: AtlasLoot first.
	first, second := res.Addons[0], res.Addons[1]
	if first.Folder != "AtlasLoot" || second.Folder != "Questie-main" {
		t.Errorf("order = %s, %s; want AtlasLoot, Questie-main", first.Folder, second.Folder)
	}
	if !first.Ignored || first.Pinned {
		t.Errorf("AtlasLoot flags = pinned %v / ignored %v, want ignored only", first.Pinned, first.Ignored)
	}
	if !second.Pinned || second.Ignored {
		t.Errorf("Questie-main flags = pinned %v / ignored %v, want pinned only", second.Pinned, second.Ignored)
	}
	if second.Provider != "github" || second.ID != "Questie/Questie" || second.Source != "Questie/Questie" {
		t.Errorf("Questie-main entry = %+v", second)
	}
	if _, err := time.Parse(time.RFC3339, second.InstalledAt); err != nil {
		t.Errorf("InstalledAt %q is not RFC3339: %v", second.InstalledAt, err)
	}
}

func TestSetAddonPinnedAndIgnored(t *testing.T) {
	s, _, registryPath, _ := newTrackedTestService(t)
	reg, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie-main", Title: "Questie", Version: "1.12.2", Provider: "github", ID: "Questie/Questie", Source: "Questie/Questie"})

	// Case-insensitive folder matching.
	if err := s.SetAddonPinned("questie-main", true); err != nil {
		t.Fatalf("SetAddonPinned: %v", err)
	}
	if err := s.SetAddonIgnored("Questie-main", true); err != nil {
		t.Fatalf("SetAddonIgnored: %v", err)
	}

	reloaded, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := reloaded.Entries()
	if len(entries) != 1 || !entries[0].Pinned || !entries[0].Ignored {
		t.Errorf("flags not persisted: %+v", entries)
	}

	// Unknown folder is a Go error.
	if err := s.SetAddonPinned("Nope", true); err == nil {
		t.Fatal("SetAddonPinned on untracked folder should error")
	} else if !strings.Contains(err.Error(), "not tracked in the registry") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestRollbackAddon runs the full rollback flow: a snapshot of the
// folder exists, the folder is modified, RollbackAddon restores the
// snapshot content, refreshes the registry entry (TOC version and
// checksum) and pins it.
func TestRollbackAddon(t *testing.T) {
	s, addonsDir, registryPath, backupsDir := newTrackedTestService(t)
	reg, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{
		Folder: "Questie-main", Title: "Questie", Version: "9.9.9",
		Provider: "github", ID: "Questie/Questie", Source: "Questie/Questie", Checksum: "stale",
	})

	folder := filepath.Join(addonsDir, "Questie-main")
	backups := backup.New(backupsDir, nil)
	// Seed the snapshot with the fixture content (TOC version 1.12.2).
	if _, err := backups.Backup([]string{folder}, "seed"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Modify the folder: bump the TOC, add a file.
	tocPath := filepath.Join(folder, "Questie.toc")
	data, err := os.ReadFile(tocPath)
	if err != nil {
		t.Fatal(err)
	}
	modified := strings.Replace(string(data), "## Version: 1.12.2", "## Version: 9.9.9", 1)
	if err := os.WriteFile(tocPath, []byte(modified), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "extra.lua"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.RollbackAddon("questie-main") // case-insensitive folder
	if err != nil {
		t.Fatalf("RollbackAddon: %v", err)
	}
	if res.Folder != "Questie-main" {
		t.Errorf("folder = %q, want Questie-main", res.Folder)
	}
	if res.RestoredFrom == "" {
		t.Error("RestoredFrom is empty")
	}
	if res.Version != "1.12.2" {
		t.Errorf("version = %q, want 1.12.2 (read back from the restored TOC)", res.Version)
	}
	if !res.Pinned {
		t.Error("Pinned = false, want true (rollback auto-pins)")
	}

	// Old content is back, the added file is gone.
	restored, err := os.ReadFile(tocPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "## Version: 1.12.2") {
		t.Errorf("restored TOC = %q, want 1.12.2", restored)
	}
	if _, err := os.Stat(filepath.Join(folder, "extra.lua")); !os.IsNotExist(err) {
		t.Error("file added after the snapshot still present")
	}

	// Registry entry refreshed and pinned (reload: RollbackAddon saves
	// through its own registry instance).
	reloaded, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := reloaded.Entries()
	if len(entries) != 1 {
		t.Fatalf("registry entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if !e.Pinned {
		t.Errorf("entry not pinned: %+v", e)
	}
	if e.Version != "1.12.2" {
		t.Errorf("entry version = %q, want 1.12.2", e.Version)
	}
	want, err := catalog.ComputeManifest(folder)
	if err != nil {
		t.Fatal(err)
	}
	if e.Checksum != want {
		t.Errorf("entry checksum = %q, want recomputed %q", e.Checksum, want)
	}
	if e.Provider != "github" || e.ID != "Questie/Questie" || e.Source != "Questie/Questie" {
		t.Errorf("entry lost its provider/source: %+v", e)
	}

	// The pre-rollback snapshot of the modified state exists.
	snapshots, err := backups.List()
	if err != nil {
		t.Fatal(err)
	}
	foundPre := false
	for _, sn := range snapshots {
		if strings.HasPrefix(sn.Reason, "pre-rollback of ") {
			foundPre = true
		}
	}
	if !foundPre {
		t.Error("no pre-rollback snapshot of the destination")
	}
}

func TestRollbackAddonUntracked(t *testing.T) {
	s, _, _, _ := newTrackedTestService(t)
	_, err := s.RollbackAddon("Nope")
	if err == nil {
		t.Fatal("RollbackAddon on an untracked folder should error")
	}
	if !strings.Contains(err.Error(), `addon "Nope" not tracked in registry`) {
		t.Errorf("error = %q, want not-tracked message", err.Error())
	}
}

func TestRollbackAddonNoSnapshot(t *testing.T) {
	s, _, registryPath, _ := newTrackedTestService(t)
	reg, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie-main", Title: "Questie", Version: "1.12.2", Provider: "github", ID: "Questie/Questie", Source: "Questie/Questie"})

	_, err = s.RollbackAddon("Questie-main")
	if err == nil {
		t.Fatal("RollbackAddon with no snapshot should error")
	}
	if !strings.Contains(err.Error(), "no backup snapshot contains") {
		t.Errorf("error = %q, want no-snapshot message", err.Error())
	}
}

// TestListAddonVersions returns the recorded log newest-first with the
// current version, mapping refs and RFC3339 timestamps.
func TestListAddonVersions(t *testing.T) {
	s, _, regPath, _ := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.0.0", Provider: "github", ID: "acme/questie", Source: "acme/questie", VersionRef: "v9.0.0"})
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.2.0", Provider: "github", ID: "acme/questie", Source: "acme/questie", VersionRef: "v9.2.0"})

	res, err := s.ListAddonVersions("questie") // case-insensitive
	if err != nil {
		t.Fatalf("ListAddonVersions: %v", err)
	}
	if res.Folder != "Questie" || res.Current != "9.2.0" {
		t.Errorf("result = folder %q current %q, want Questie / 9.2.0", res.Folder, res.Current)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("versions = %d, want 2: %+v", len(res.Versions), res.Versions)
	}
	if res.Versions[0].Version != "9.2.0" || res.Versions[1].Version != "9.0.0" {
		t.Errorf("versions order = [%s, %s], want [9.2.0, 9.0.0]", res.Versions[0].Version, res.Versions[1].Version)
	}
	if res.Versions[0].Ref != "v9.2.0" || res.Versions[0].Provider != "github" || res.Versions[0].Source != "acme/questie" {
		t.Errorf("version entry = %+v", res.Versions[0])
	}
	if _, err := time.Parse(time.RFC3339, res.Versions[0].At); err != nil {
		t.Errorf("at = %q is not RFC3339: %v", res.Versions[0].At, err)
	}
}

func TestListAddonVersionsUntracked(t *testing.T) {
	s, _, _, _ := newTestCatalogService(t)
	_, err := s.ListAddonVersions("Nope")
	if err == nil {
		t.Fatal("ListAddonVersions on an untracked folder should error")
	}
	if !strings.Contains(err.Error(), "not tracked in registry") {
		t.Errorf("error = %q, want not-tracked message", err.Error())
	}
}

// TestTrackedAddonsReportsHistory checks the has_history flag drives
// the History menu state: version changes record history, and
// old-format entries (no history field) report false.
func TestTrackedAddonsReportsHistory(t *testing.T) {
	s, _, regPath, _ := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.0.0", Provider: "github", ID: "acme/questie"})
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.2.0", Provider: "github", ID: "acme/questie"})
	// A fresh install records its initial version too.
	_ = reg.Track(catalog.Entry{Folder: "Atlas", Title: "Atlas", Version: "7.0.4", Provider: "github", ID: "acme/atlas"})

	res, err := s.TrackedAddons()
	if err != nil {
		t.Fatalf("TrackedAddons: %v", err)
	}
	byFolder := map[string]TrackedAddon{}
	for _, a := range res.Addons {
		byFolder[a.Folder] = a
	}
	if !byFolder["Questie"].HasHistory {
		t.Error("Questie: HasHistory = false, want true")
	}
	if !byFolder["Atlas"].HasHistory {
		t.Error("Atlas: HasHistory = false, want true (initial install recorded)")
	}

	// An old-format entry without a history field reports false.
	if err := os.WriteFile(regPath, []byte(`{
  "version": 1,
  "entries": [
    {"folder": "Legacy", "title": "Legacy", "version": "1.0.0", "provider": "github", "id": "acme/legacy", "installed_at": "2024-01-02T03:04:05Z"}
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = s.TrackedAddons()
	if err != nil {
		t.Fatalf("TrackedAddons (old format): %v", err)
	}
	for _, a := range res.Addons {
		if a.Folder == "Legacy" && a.HasHistory {
			t.Error("Legacy (old format): HasHistory = true, want false")
		}
	}
}

// TestRollbackToVersion re-downloads a specific GitHub tag, replaces
// the folder, snapshots it first and re-records the registry with a
// fresh history entry.
func TestRollbackToVersion(t *testing.T) {
	s, addonsDir, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	mock.repos["acme/questie"] = "v9.2.0"
	mock.tags = map[string]bool{"acme/questie/v9.0.0": true}
	mock.zips["acme/questie"] = addonZipBytes(t, "Questie", "## Title: Questie\n## Version: 9.0.0\n## Interface: 30300\n")
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.0.0", Provider: "github", ID: "acme/questie", Source: "acme/questie", VersionRef: "v9.0.0"})
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.2.0", Provider: "github", ID: "acme/questie", Source: "acme/questie", VersionRef: "v9.2.0"})

	res, err := s.RollbackToVersion("Questie", "9.0.0")
	if err != nil {
		t.Fatalf("RollbackToVersion: %v", err)
	}
	if len(res.Installed) != 1 || res.Installed[0] != "Questie" {
		t.Errorf("result = %+v, want Questie installed", res)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none", res.Errors)
	}

	// The folder carries the rolled-back TOC version.
	toc, err := os.ReadFile(filepath.Join(addonsDir, "Questie", "Questie.toc"))
	if err != nil {
		t.Fatalf("read TOC: %v", err)
	}
	if !strings.Contains(string(toc), "9.0.0") {
		t.Errorf("TOC after rollback = %q, want 9.0.0", toc)
	}

	// The replaced folder was snapshotted first (safety path).
	root := filepath.Clean(filepath.Join(addonsDir, "..", ".."))
	entries, err := os.ReadDir(filepath.Join(root, "Backups"))
	if err != nil {
		t.Fatalf("backups dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no backup snapshot of the replaced folder")
	}

	// The registry records the rollback with a fresh history entry.
	reloaded, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	e := reloaded.Entries()[0]
	if e.Version != "9.0.0" {
		t.Errorf("tracked version = %q, want 9.0.0", e.Version)
	}
	if e.Provider != "github" || e.ID != "acme/questie" || e.Source != "acme/questie" {
		t.Errorf("entry provider fields not preserved: %+v", e)
	}
	if len(e.History) != 3 || e.History[0].Version != "9.0.0" {
		t.Fatalf("history after rollback = %+v, want [9.0.0, 9.2.0, 9.0.0]", e.History)
	}
	if e.History[0].Ref != "v9.0.0" {
		t.Errorf("history ref = %q, want v9.0.0", e.History[0].Ref)
	}
}

// TestRollbackToVersionNotServed fails honestly when the provider can
// only serve the latest version.
func TestRollbackToVersionNotServed(t *testing.T) {
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	writeFixture(t, addonsDir)

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s := New(store)
	s.registryPath = filepath.Join(t.TempDir(), "registry.json")
	s.enabledProviders = map[string]bool{catalog.ProviderWowInterface: true}
	reg, err := catalog.NewRegistry(s.registryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.2.0", Provider: "wowinterface", ID: "12345", Source: "https://www.wowinterface.com/downloads/info12345-Questie.html"})
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.0.0", Provider: "wowinterface", ID: "12345", Source: "https://www.wowinterface.com/downloads/info12345-Questie.html"})

	_, err = s.RollbackToVersion("Questie", "9.0.0")
	if err == nil {
		t.Fatal("RollbackToVersion on WowInterface should error")
	}
	if !errors.Is(err, catalog.ErrVersionNotServed) {
		t.Errorf("error should wrap catalog.ErrVersionNotServed, got %v", err)
	}
	if !strings.Contains(err.Error(), "wowinterface") || !strings.Contains(err.Error(), "only the latest is available") {
		t.Errorf("error = %q, want the honest not-served message", err.Error())
	}
}

// TestRollbackToVersionUnknownVersion errors when the requested
// version was never recorded for the addon.
func TestRollbackToVersionUnknownVersion(t *testing.T) {
	s, _, regPath, _ := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie", Title: "Questie", Version: "9.2.0", Provider: "github", ID: "acme/questie", Source: "acme/questie"})

	_, err = s.RollbackToVersion("Questie", "1.0.0")
	if err == nil {
		t.Fatal("RollbackToVersion with an unrecorded version should error")
	}
	if !strings.Contains(err.Error(), `no recorded version "1.0.0"`) {
		t.Errorf("error = %q, want no-recorded-version message", err.Error())
	}
}

// TestRollbackToVersionUntracked errors for an unknown folder.
func TestRollbackToVersionUntracked(t *testing.T) {
	s, _, _, _ := newTestCatalogService(t)
	_, err := s.RollbackToVersion("Nope", "1.0.0")
	if err == nil {
		t.Fatal("RollbackToVersion on an untracked folder should error")
	}
	if !strings.Contains(err.Error(), "not tracked in registry") {
		t.Errorf("error = %q, want not-tracked message", err.Error())
	}
}

// TestScanReportsPinAndIgnore checks the scan DTO enrichment: tracked
// addons carry their registry flags, untracked ones report false.
func TestScanReportsPinAndIgnore(t *testing.T) {
	s, _, registryPath, _ := newTrackedTestService(t)
	reg, err := catalog.NewRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Questie-main", Title: "Questie", Version: "1.12.2", Provider: "github", ID: "Questie/Questie", Source: "Questie/Questie", Pinned: true})
	_ = reg.Track(catalog.Entry{Folder: "AtlasLoot", Title: "AtlasLoot", Version: "7.0.4", Provider: "curseforge", ID: "atlasloot", Source: "https://www.curseforge.com/wow/addons/atlasloot", Ignored: true})

	res, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byFolder := map[string]Addon{}
	for _, a := range res.Addons {
		byFolder[a.FolderName] = a
	}
	questie, ok := byFolder["Questie-main"]
	if !ok {
		t.Fatal("Questie-main not in scan results")
	}
	if !questie.Tracked || !questie.Pinned || questie.Ignored {
		t.Errorf("Questie-main = tracked %v / pinned %v / ignored %v, want tracked+pinned only",
			questie.Tracked, questie.Pinned, questie.Ignored)
	}
	atlas, ok := byFolder["AtlasLoot"]
	if !ok {
		t.Fatal("AtlasLoot not in scan results")
	}
	if !atlas.Tracked || atlas.Pinned || !atlas.Ignored {
		t.Errorf("AtlasLoot = tracked %v / pinned %v / ignored %v, want tracked+ignored only",
			atlas.Tracked, atlas.Pinned, atlas.Ignored)
	}
	aux, ok := byFolder["AuxUI"] // untracked
	if !ok {
		t.Fatal("AuxUI not in scan results")
	}
	if aux.Tracked || aux.Pinned || aux.Ignored {
		t.Errorf("AuxUI (untracked) = tracked %v / pinned %v / ignored %v, want all false",
			aux.Tracked, aux.Pinned, aux.Ignored)
	}
}

// TestExportSnapshot freezes the tracked addon state plus the latest
// versions resolved through the mock provider into the snapshot DTO;
// pinned entries keep their flag and per-addon lookup failures land in
// Warnings instead of aborting the export.
func TestExportSnapshot(t *testing.T) {
	s, _, regPath, mock := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Alpha", Title: "Alpha", Version: "1.0.0",
		Provider: "github", ID: "acme/alpha", Source: "acme/alpha"})
	_ = reg.Track(catalog.Entry{Folder: "Beta", Title: "Beta", Version: "2.0.0", Pinned: true,
		Provider: "github", ID: "acme/beta", Source: "acme/beta"})
	_ = reg.Track(catalog.Entry{Folder: "Broken", Title: "Broken", Version: "1.0.0",
		Provider: "github", ID: "acme/broken", Source: "acme/broken"})
	mock.repos["acme/alpha"] = "v2.0.0"
	mock.repos["acme/beta"] = "v2.1.0"
	// acme/broken has no repository metadata: the lookup fails.

	res, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if res.AddonCount != 3 {
		t.Fatalf("addon_count = %d, want 3", res.AddonCount)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Broken") {
		t.Fatalf("warnings = %v, want the Broken lookup failure", res.Warnings)
	}
	if _, err := time.Parse(time.RFC3339, res.ExportedAt); err != nil {
		t.Errorf("exported_at %q is not RFC3339: %v", res.ExportedAt, err)
	}
	snap, err := catalog.UnmarshalSnapshot([]byte(res.SnapshotJSON))
	if err != nil {
		t.Fatalf("snapshot_json does not parse: %v", err)
	}
	if snap.Profile != "wrath" {
		t.Errorf("profile = %q, want wrath", snap.Profile)
	}
	byFolder := map[string]catalog.SnapshotAddon{}
	for _, a := range snap.Addons {
		byFolder[a.Folder] = a
	}
	if a := byFolder["Alpha"]; a.LatestVersion != "v2.0.0" || a.InstalledVersion != "1.0.0" {
		t.Errorf("Alpha = %+v, want resolved latest v2.0.0", a)
	}
	if a := byFolder["Beta"]; !a.Pinned || a.LatestVersion != "v2.1.0" {
		t.Errorf("Beta = %+v, want pinned with resolved latest", a)
	}
	if a := byFolder["Broken"]; a.LatestVersion != "" {
		t.Errorf("Broken latest = %q, want empty on lookup failure", a.LatestVersion)
	}
}

// TestCheckSnapshot diffs a snapshot against the live registry with
// no network traffic (the mock is deliberately left empty) and maps
// the updates into the UpdateInfo DTO shape; pinned entries are
// skipped.
func TestCheckSnapshot(t *testing.T) {
	s, _, regPath, _ := newTestCatalogService(t)
	reg, err := catalog.NewRegistry(regPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = reg.Track(catalog.Entry{Folder: "Alpha", Title: "Alpha", Version: "1.0.0",
		Provider: "github", ID: "acme/alpha", Source: "acme/alpha"})
	_ = reg.Track(catalog.Entry{Folder: "Pinned", Title: "Pinned", Version: "1.0.0", Pinned: true,
		Provider: "github", ID: "acme/pinned", Source: "acme/pinned"})

	snap := &catalog.Snapshot{
		Version: 1,
		Profile: "wrath",
		Addons: []catalog.SnapshotAddon{
			{Folder: "Alpha", Provider: "github", ID: "acme/alpha", Source: "acme/alpha",
				InstalledVersion: "1.0.0", LatestVersion: "v2.0.0"},
			{Folder: "Pinned", Provider: "github", ID: "acme/pinned", Source: "acme/pinned",
				InstalledVersion: "1.0.0", LatestVersion: "v9.0.0"},
		},
	}
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.CheckSnapshot(string(data))
	if err != nil {
		t.Fatalf("CheckSnapshot: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors = %v, want none", res.Errors)
	}
	if len(res.Updates) != 1 {
		t.Fatalf("updates = %d, want 1: %+v", len(res.Updates), res.Updates)
	}
	u := res.Updates[0]
	if u.Folder != "Alpha" || u.CurrentVersion != "1.0.0" || u.LatestVersion != "v2.0.0" ||
		u.Provider != "github" || u.ID != "acme/alpha" || u.Source != "acme/alpha" {
		t.Errorf("update = %+v", u)
	}
	if u.FlavorMismatch {
		t.Errorf("flavor_mismatch = true, want false")
	}
}

// TestCheckSnapshotBadJSON surfaces malformed snapshot input as a
// clear Go error.
func TestCheckSnapshotBadJSON(t *testing.T) {
	s, _, _, _ := newTestCatalogService(t)
	if _, err := s.CheckSnapshot("{not json"); err == nil {
		t.Error("bad snapshot JSON should error")
	}
}

// Wago fixtures mirror internal/catalog/wago_test.go: the check body
// for the resolved import and the raw encoded import string the
// encoded endpoint serves.
const wagoCheckFixture = `[
	{
		"_id": "pvBs8htuW",
		"name": "Sea Swell - CountDown Move Bar",
		"username": "DoGGyFromPlanetWoof",
		"game": "tww",
		"type": "WEAKAURA",
		"version": 2,
		"versionString": "1.0.1",
		"url": "https://wago.io/pvBs8htuW",
		"modified": "2024-11-14T09:30:00Z"
	}
]`

const wagoImportString = "!DEvBZjUnq4FnDM(LJy78YLPFdsGEmdbsrox6DdJ5ewcqnglxj582hUF7D3vYyt6LRmntgW6flT7ZUpp7swCwAgBxgtKXSzSKEXX9ofhbZQY1LlTkHmJnz4iycH0YD1gUtMTkJLR1fc9tLPYNDdl5RkKISbWzPfYILpNnncor5UhLMmwCVOEXzmNAN0WuVkZME6fWQoE(d2R0fAdCDtJP)tOppL(8m8txg7jLWTfggbhP2OKLoUtPlZyFA28XFD200(tGtRIBEyqHSuCJgn5(xFn4cLoPPKx8zPXIVX0y0ma"

// mockWagoServer serves the data.wago.io endpoints the way the real
// API does: the check body for /api/check and a 302-then-body for the
// encoded endpoint. Traffic reaches it through wagoRewriteTransport,
// mirroring the mockGitHub pattern.
func mockWagoServer(t *testing.T) *http.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/check/":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(wagoCheckFixture))
		case "/api/raw/encoded":
			if r.URL.Query().Get("version") == "" {
				http.Redirect(w, r, "/api/raw/encoded?id="+r.URL.Query().Get("id")+"&version=1.0.1", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(wagoImportString))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return &http.Client{
		Transport: &wagoRewriteTransport{mock: strings.TrimPrefix(ts.URL, "http://"), base: ts.Client().Transport},
	}
}

// wagoRewriteTransport redirects data.wago.io traffic to a mock server
// so the real Wago API is never touched.
type wagoRewriteTransport struct {
	mock string // mock origin "host:port"
	base http.RoundTripper
}

func (t *wagoRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "data.wago.io" {
		return nil, fmt.Errorf("test transport refuses non-wago host %s", req.URL.Host)
	}
	r := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = t.mock
	r.URL = &u
	return t.base.RoundTrip(r)
}

// newTestWagoService wires a Service to a fake install, an isolated
// registry, a wago-only catalog whose traffic hits a mock
// data.wago.io server and a temp wagoDir override.
func newTestWagoService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	writeFixture(t, addonsDir)

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s := New(store)
	s.registryPath = filepath.Join(t.TempDir(), "registry.json")
	s.enabledProviders = map[string]bool{catalog.ProviderWago: true}
	s.wagoDirOverride = t.TempDir()
	s.httpClient = mockWagoServer(t)
	return s
}

// TestSaveWagoImport downloads an import string through the wago
// provider into the WagoImports folder and checks the file content,
// the sanitized file name and the DTO fields.
func TestSaveWagoImport(t *testing.T) {
	s := newTestWagoService(t)
	res, err := s.SaveWagoImport("pvBs8htuW")
	if err != nil {
		t.Fatalf("SaveWagoImport: %v", err)
	}
	wantPath := filepath.Join(s.wagoDirOverride, "WagoImports", "Sea_Swell_-_CountDown_Move_Bar.txt")
	if res.Path != wantPath {
		t.Errorf("path = %q, want %q", res.Path, wantPath)
	}
	if res.Name != "Sea Swell - CountDown Move Bar" {
		t.Errorf("name = %q", res.Name)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("import file not written: %v", err)
	}
	if string(data) != wagoImportString {
		t.Errorf("file holds %d bytes, want the raw import string", len(data))
	}
	if res.Bytes != len(wagoImportString) {
		t.Errorf("bytes = %d, want %d", res.Bytes, len(wagoImportString))
	}
	if !strings.Contains(res.AppliedHint, wantPath) || !strings.Contains(res.AppliedHint, "WeakAuras → Import") {
		t.Errorf("applied_hint = %q", res.AppliedHint)
	}
}

// TestSaveWagoImportCollision verifies the -2/-3 dedup suffix on
// name collisions, mirroring backup naming.
func TestSaveWagoImportCollision(t *testing.T) {
	s := newTestWagoService(t)
	if _, err := s.SaveWagoImport("pvBs8htuW"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	res, err := s.SaveWagoImport("pvBs8htuW")
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	wantPath := filepath.Join(s.wagoDirOverride, "WagoImports", "Sea_Swell_-_CountDown_Move_Bar-2.txt")
	if res.Path != wantPath {
		t.Errorf("path = %q, want %q", res.Path, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("deduped file missing: %v", err)
	}
}

// TestSaveWagoImportProviderDisabled surfaces a disabled wago
// provider as a clear Go error.
func TestSaveWagoImportProviderDisabled(t *testing.T) {
	s := newTestWagoService(t)
	s.enabledProviders = map[string]bool{}
	res, err := s.SaveWagoImport("pvBs8htuW")
	if err == nil || !strings.Contains(err.Error(), "wago provider is not enabled") {
		t.Fatalf("err = %v, want disabled-provider error", err)
	}
	if res != (WagoImportResult{}) {
		t.Errorf("res = %+v, want zero value", res)
	}
}

// TestSaveWagoImportEmptyID requires a non-empty id before any
// network traffic.
func TestSaveWagoImportEmptyID(t *testing.T) {
	s := newTestWagoService(t)
	if _, err := s.SaveWagoImport(""); err == nil || !strings.Contains(err.Error(), "missing wago import id") {
		t.Fatalf("empty id err = %v, want clear error", err)
	}
	if _, err := s.SaveWagoImport("   "); err == nil {
		t.Fatal("whitespace id should error")
	}
}

// curatedTestService wires a Service to an empty fake install under
// the given profile id.
func curatedTestService(t *testing.T, profileID string) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	cfg.Profile = profileID
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return New(store), addonsDir
}

func writeCuratedTOC(t *testing.T, addonsDir, folder, toc string) {
	t.Helper()
	dir := filepath.Join(addonsDir, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, folder+".toc"), []byte(toc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCurated selects the embedded set by the profile's family and
// annotates install state from the AddOns directory.
func TestCurated(t *testing.T) {
	s, addonsDir := curatedTestService(t, "wrath")

	res, err := s.Curated()
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	if res.Family != "wrath" || res.ProfileID != "wrath" {
		t.Errorf("family/profile = %q/%q, want wrath/wrath", res.Family, res.ProfileID)
	}
	if res.Label == "" || len(res.Addons) == 0 {
		t.Fatalf("empty curated set: %+v", res)
	}
	for _, a := range res.Addons {
		if a.Name == "" || a.Source == "" || a.Summary == "" || a.Homepage == "" {
			t.Errorf("incomplete row %+v", a)
		}
		if a.Installed {
			t.Errorf("%s reported installed with no AddOns content", a.Name)
		}
	}

	// A matching folder with a TOC flips Installed and surfaces the
	// TOC version. "WeakAuras" satisfies the WeakAuras-WotLK entry
	// through the suffix-stripping rule, but "Bartender4" stays
	// untouched.
	writeCuratedTOC(t, addonsDir, "WeakAuras", "## Interface: 30300\n## Title: WeakAuras\n## Version: 5.12.10\n")

	res, err = s.Curated()
	if err != nil {
		t.Fatalf("Curated after install: %v", err)
	}
	found := map[string]CuratedAddon{}
	for _, a := range res.Addons {
		found[a.Name] = a
	}
	if a, ok := found["WeakAuras-WotLK"]; !ok || !a.Installed || a.InstalledVersion != "5.12.10" {
		t.Errorf("WeakAuras-WotLK row = %+v, want installed 5.12.10", a)
	}
	if a, ok := found["Bartender4"]; ok && a.Installed {
		t.Errorf("Bartender4 row = %+v, want not installed", a)
	}
}

// TestCuratedVanillaPrefixRule checks the suffix-stripping match on
// the vanilla set: a "pfQuest" folder satisfies both the pfQuest and
// the pfQuest-turtle entries.
func TestCuratedVanillaPrefixRule(t *testing.T) {
	s, addonsDir := curatedTestService(t, "turtle")
	writeCuratedTOC(t, addonsDir, "pfQuest", "## Interface: 11200\n## Title: pfQuest\n## Version: 7.0.1\n")

	res, err := s.Curated()
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	found := map[string]CuratedAddon{}
	for _, a := range res.Addons {
		found[a.Name] = a
	}
	if a, ok := found["pfQuest-turtle"]; !ok || !a.Installed || a.InstalledVersion != "7.0.1" {
		t.Errorf("pfQuest-turtle row = %+v, want installed 7.0.1", a)
	}
	if a, ok := found["pfQuest"]; !ok || !a.Installed || a.InstalledVersion != "7.0.1" {
		t.Errorf("pfQuest row = %+v, want installed 7.0.1", a)
	}
	if a, ok := found["pfUI"]; ok && a.Installed {
		t.Errorf("pfUI row = %+v, want not installed", a)
	}
}

// TestCuratedRepoBaseMatch detects a folder that matches the source's
// repo name rather than the addon name (e.g. the AtlasLoot_ChromieCraft
// zip installs "AtlasLoot").
func TestCuratedRepoBaseMatch(t *testing.T) {
	s, addonsDir := curatedTestService(t, "wrath")
	writeCuratedTOC(t, addonsDir, "AtlasLoot", "## Interface: 30300\n## Title: AtlasLoot\n## Version: 5.11.04\n")

	res, err := s.Curated()
	if err != nil {
		t.Fatalf("Curated: %v", err)
	}
	for _, a := range res.Addons {
		if a.Name == "AtlasLoot_ChromieCraft" && (!a.Installed || a.InstalledVersion != "5.11.04") {
			t.Errorf("AtlasLoot_ChromieCraft row = %+v, want installed 5.11.04", a)
		}
	}
}

// TestCuratedVanillaProfile maps turtle and classic profiles onto the
// vanilla family.
func TestCuratedVanillaProfile(t *testing.T) {
	for _, id := range []string{"turtle", "classic"} {
		s, _ := curatedTestService(t, id)
		res, err := s.Curated()
		if err != nil {
			t.Fatalf("Curated(%s): %v", id, err)
		}
		if res.Family != "vanilla" {
			t.Errorf("profile %s: family = %q, want vanilla", id, res.Family)
		}
		if len(res.Addons) == 0 {
			t.Errorf("profile %s: empty vanilla set", id)
		}
	}
}

// TestCuratedNoSetForUnknownFamily returns an empty list with a nil
// error so the frontend shows nothing for tbc/cata/retail profiles.
func TestCuratedNoSetForUnknownFamily(t *testing.T) {
	s, _ := curatedTestService(t, "tbc")
	res, err := s.Curated()
	if err != nil {
		t.Fatalf("Curated(tbc): %v", err)
	}
	if res.Family != "tbc" || len(res.Addons) != 0 {
		t.Errorf("tbc result = %+v, want empty addons", res)
	}
}

// ---------------------------------------------------------------------------
// Service contract: info / sources / doctor / savedvars / backups / export /
// import / config. Every method is exercised against temp trees only.
// ---------------------------------------------------------------------------

// TestServiceAddonInfoFromSource resolves a github "owner/repo" source
// through the mock provider and expects the resolved fields plus the
// stripped GitHub release notes.
func TestServiceAddonInfoFromSource(t *testing.T) {
	s, _, _, mock := newTestCatalogService(t)
	mock.repos["acme/newaddon"] = "v1.0.0"
	mock.notes["acme/newaddon"] = "## v1.0.0\n\n- **fixed** a bug\n- see `https://example.test`"

	res, err := s.AddonInfo("acme/newaddon")
	if err != nil {
		t.Fatalf("AddonInfo: %v", err)
	}
	if res.Provider != "github" || res.ID != "acme/newaddon" {
		t.Errorf("provider/id = %q/%q, want github/acme/newaddon", res.Provider, res.ID)
	}
	if res.Name != "newaddon" || res.Author != "acme" {
		t.Errorf("name/author = %q/%q", res.Name, res.Author)
	}
	if res.LatestVersion != "v1.0.0" {
		t.Errorf("latest_version = %q, want v1.0.0", res.LatestVersion)
	}
	if !strings.Contains(res.ReleaseNotes, "fixed a bug") || !strings.Contains(res.ReleaseNotes, "https://example.test") {
		t.Errorf("release_notes = %q, want markdown stripped to plain text", res.ReleaseNotes)
	}
	if res.Matches != nil {
		t.Errorf("matches = %v, want nil for a resolved source", res.Matches)
	}
}

// TestServiceAddonInfoUnknownProvider errors when the classified
// provider is not enabled in the catalog.
func TestServiceAddonInfoUnknownProvider(t *testing.T) {
	s, _, _, _ := newTestCatalogService(t)
	_, err := s.AddonInfo("https://www.wowinterface.com/downloads/info9999-Foo.html")
	if err == nil {
		t.Fatal("AddonInfo with a disabled provider should error")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("err = %q, want provider-not-available", err.Error())
	}
}

// TestServiceAddonInfoBareNameSingleMatch resolves the unique search
// hit directly, without a second provider round-trip.
func TestServiceAddonInfoBareNameSingleMatch(t *testing.T) {
	s, _, _, mock := newTestCatalogService(t)
	mock.results = []string{"acme/newaddon"}
	mock.repos["acme/newaddon"] = "v1.0.0"

	res, err := s.AddonInfo("newaddon")
	if err != nil {
		t.Fatalf("AddonInfo: %v", err)
	}
	if res.Provider != "github" || res.ID != "acme/newaddon" {
		t.Errorf("provider/id = %q/%q, want github/acme/newaddon", res.Provider, res.ID)
	}
	if res.Name != "newaddon" {
		t.Errorf("name = %q, want newaddon", res.Name)
	}
	if res.Matches != nil {
		t.Errorf("matches = %v, want nil for a unique match", res.Matches)
	}
}

// TestServiceAddonInfoBareNameAmbiguous returns the candidate matches
// with a nil error so the frontend can ask the user which one.
func TestServiceAddonInfoBareNameAmbiguous(t *testing.T) {
	s, _, _, mock := newTestCatalogService(t)
	mock.results = []string{"xperl/xperl", "flux/flux"}

	res, err := s.AddonInfo("x")
	if err != nil {
		t.Fatalf("AddonInfo: %v", err)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("matches = %d, want 2: %+v", len(res.Matches), res.Matches)
	}
	names := map[string]bool{}
	for _, m := range res.Matches {
		names[m.Name] = true
		if m.Provider != "github" {
			t.Errorf("match %q provider = %q, want github", m.Name, m.Provider)
		}
	}
	if !names["xperl"] || !names["flux"] {
		t.Errorf("matches = %v, want xperl and flux", res.Matches)
	}
	if res.Provider != "" || res.Name != "" {
		t.Errorf("resolved fields should stay empty on ambiguity, got %+v", res)
	}
}

// TestServiceAddonInfoBareNameNoMatch errors for an unknown addon.
func TestServiceAddonInfoBareNameNoMatch(t *testing.T) {
	s, _, _, _ := newTestCatalogService(t)
	_, err := s.AddonInfo("zzz-no-such-addon")
	if err == nil {
		t.Fatal("AddonInfo with no matches should error")
	}
	if !strings.Contains(err.Error(), "no matches") {
		t.Errorf("err = %q, want no-matches error", err.Error())
	}
}

// TestServiceSources lists the four providers with their exact
// descriptions, in order.
func TestServiceSources(t *testing.T) {
	s, _ := newTestService(t)
	srcs, err := s.Sources()
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	want := []ProviderInfo{
		{Name: "github", Description: "GitHub releases API — unauthenticated, ~60 requests/hour"},
		{Name: "curseforge", Description: "modern Core API with WOWFIX_CURSEFORGE_API_KEY, else deprecated legacy endpoint"},
		{Name: "wowinterface", Description: "MMOUI filelist JSON"},
		{Name: "tukui", Description: "tukui.org API"},
	}
	if len(srcs) != len(want) {
		t.Fatalf("sources = %d, want %d", len(srcs), len(want))
	}
	for i := range want {
		if srcs[i] != want[i] {
			t.Errorf("source[%d] = %+v, want %+v", i, srcs[i], want[i])
		}
	}
}

// TestServiceDoctor checks the report structure: one check per doctor
// line, valid statuses, and the deterministic verdicts of a healthy
// temp install.
func TestServiceDoctor(t *testing.T) {
	s, _ := newTestService(t)
	s.registryPath = filepath.Join(t.TempDir(), "registry.json")

	rep, err := s.Doctor()
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	byName := map[string]DoctorCheck{}
	for _, c := range rep.Checks {
		if _, dup := byName[c.Name]; dup {
			t.Errorf("duplicate doctor check %q", c.Name)
		}
		byName[c.Name] = c
		switch c.Status {
		case "ok", "warn", "error", "info":
		default:
			t.Errorf("check %q has invalid status %q", c.Name, c.Status)
		}
	}
	for _, want := range []string{
		"config", "profile", "theme", "collection", "install", "flavor",
		"permissions", "scan", "backups", "trash", "registry", "collections", "savedvars",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("doctor check %q missing", want)
		}
	}
	if c := byName["profile"]; c.Status != "ok" || c.Message != "wrath" {
		t.Errorf("profile check = %+v, want ok wrath", c)
	}
	if c := byName["theme"]; c.Status != "ok" || c.Message != "dark" {
		t.Errorf("theme check = %+v, want ok dark", c)
	}
	if c := byName["collection"]; c.Status != "ok" {
		t.Errorf("collection check = %+v, want ok (none set)", c)
	}
	if c := byName["permissions"]; c.Status != "ok" {
		t.Errorf("permissions check = %+v, want ok", c)
	}
	if c := byName["scan"]; !strings.Contains(c.Message, "addon(s)") {
		t.Errorf("scan check = %+v, want addon counts", c)
	}
	if c := byName["backups"]; c.Status != "info" {
		t.Errorf("backups check = %+v, want info", c)
	}
	if c := byName["trash"]; c.Status != "ok" {
		t.Errorf("trash check = %+v, want ok", c)
	}
	if c := byName["registry"]; c.Status != "info" {
		t.Errorf("registry check = %+v, want info (none yet)", c)
	}
	if c := byName["collections"]; c.Status != "warn" {
		t.Errorf("collections check = %+v, want warn (not configured)", c)
	}
	if c := byName["savedvars"]; c.Status != "error" {
		t.Errorf("savedvars check = %+v, want error (WTF missing)", c)
	}
}

// savedVarsTestService wires a Service to a fake install whose WTF
// tree holds two accounts with SavedVariables files.
func savedVarsTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	addonsDir := filepath.Join(root, "Interface", "AddOns")
	writeFixture(t, addonsDir)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, "WTF", "Account", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("A1", "SavedVariables", "DBM.lua"), "DBM = {}\n")
	write(filepath.Join("A1", "SavedVariables", "BigWigs.lua"), "BigWigs = {}\n")
	write(filepath.Join("A2", "SavedVariables", "WeakAuras.lua"), "WeakAuras = {}\n")

	store := config.NewStoreAt(filepath.Join(t.TempDir(), "config.json"))
	cfg := config.Default()
	cfg.WoWPath = root
	cfg.Flavor = ""
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return New(store), addonsDir
}

// TestServiceSavedVarsAccountsAndList lists the accounts and one
// account's files; an empty account picks the first one.
func TestServiceSavedVarsAccountsAndList(t *testing.T) {
	s, _ := savedVarsTestService(t)

	accts, err := s.SavedVarsAccounts()
	if err != nil {
		t.Fatalf("SavedVarsAccounts: %v", err)
	}
	if !slices.Equal(accts, []string{"A1", "A2"}) {
		t.Errorf("accounts = %v, want [A1 A2]", accts)
	}

	res, err := s.SavedVarsList("A1")
	if err != nil {
		t.Fatalf("SavedVarsList: %v", err)
	}
	if res.Account != "A1" {
		t.Errorf("account = %q, want A1", res.Account)
	}
	if !slices.Equal(res.Files, []string{"BigWigs", "DBM"}) {
		t.Errorf("files = %v, want [BigWigs DBM]", res.Files)
	}
	if !strings.HasSuffix(res.WtfRoot, "WTF") {
		t.Errorf("wtf_root = %q, want .../WTF", res.WtfRoot)
	}

	first, err := s.SavedVarsList("")
	if err != nil {
		t.Fatalf("SavedVarsList(\"\"): %v", err)
	}
	if first.Account != "A1" {
		t.Errorf("default account = %q, want A1", first.Account)
	}
}

// TestServiceSavedVarsBackupRestoreReset backs up an account, mutates
// the live files, restores them from the backup and resets one addon.
func TestServiceSavedVarsBackupRestoreReset(t *testing.T) {
	s, addonsDir := savedVarsTestService(t)
	root := filepath.Dir(filepath.Dir(addonsDir))
	live := filepath.Join(root, "WTF", "Account", "A1", "SavedVariables")

	back, err := s.SavedVarsBackup("A1")
	if err != nil {
		t.Fatalf("SavedVarsBackup: %v", err)
	}
	if back.Account != "A1" {
		t.Errorf("account = %q, want A1", back.Account)
	}
	if !strings.Contains(back.Path, filepath.Join("WTF", "savedvars-backups")) {
		t.Errorf("backup path = %q, want under WTF/savedvars-backups", back.Path)
	}
	if _, err := os.Stat(filepath.Join(back.Path, "DBM.lua")); err != nil {
		t.Errorf("DBM.lua missing in backup: %v", err)
	}

	if err := os.WriteFile(filepath.Join(live, "DBM.lua"), []byte("DBM = {corrupted = true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SavedVarsRestore("A1", back.Path); err != nil {
		t.Fatalf("SavedVarsRestore: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(live, "DBM.lua"))
	if err != nil {
		t.Fatalf("DBM.lua gone after restore: %v", err)
	}
	if strings.Contains(string(data), "corrupted") {
		t.Error("restore did not replace the live file")
	}

	if err := s.SavedVarsReset("A1", "DBM"); err != nil {
		t.Fatalf("SavedVarsReset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, "DBM.lua")); !os.IsNotExist(err) {
		t.Errorf("DBM.lua should be gone after reset, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, "BigWigs.lua")); err != nil {
		t.Errorf("BigWigs.lua should survive the reset: %v", err)
	}
}

// TestServiceSavedVarsMigrate copies a single addon and then the
// remaining files, never overwriting existing destinations.
func TestServiceSavedVarsMigrate(t *testing.T) {
	s, addonsDir := savedVarsTestService(t)
	root := filepath.Dir(filepath.Dir(addonsDir))

	copied, err := s.SavedVarsMigrate("A1", "A2", "DBM")
	if err != nil {
		t.Fatalf("SavedVarsMigrate(addon): %v", err)
	}
	if !slices.Equal(copied.Copied, []string{"DBM"}) {
		t.Errorf("copied = %v, want [DBM]", copied)
	}
	if _, err := os.Stat(filepath.Join(root, "WTF", "Account", "A2", "SavedVariables", "DBM.lua")); err != nil {
		t.Errorf("DBM.lua not migrated to A2: %v", err)
	}

	copied, err = s.SavedVarsMigrate("A2", "A1", "")
	if err != nil {
		t.Fatalf("SavedVarsMigrate(all): %v", err)
	}
	if !slices.Equal(copied.Copied, []string{"WeakAuras"}) {
		t.Errorf("copied = %v, want [WeakAuras] (existing files skipped)", copied)
	}
}

// TestServiceBackupNowListRestore snapshots the fixture addons, lists
// the history with manifest details and restores the snapshot in place.
func TestServiceBackupNowListRestore(t *testing.T) {
	s, _ := newTestService(t)

	bres, err := s.BackupNow()
	if err != nil {
		t.Fatalf("BackupNow: %v", err)
	}
	if bres.ID == "" {
		t.Fatal("backup id is empty")
	}

	list, err := s.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list.Snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(list.Snapshots))
	}
	snap := list.Snapshots[0]
	if snap.ID == "" {
		t.Error("snapshot id empty")
	}
	if snap.Reason != "manual backup" {
		t.Errorf("reason = %q, want %q", snap.Reason, "manual backup")
	}
	if snap.Folders != 7 {
		t.Errorf("folders = %d, want 7 (fixture addon folders)", snap.Folders)
	}
	if snap.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}

	restored, err := s.RestoreBackup(snap.ID, true)
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if len(restored.Restored) != 7 || len(restored.Skipped) != 0 {
		t.Errorf("restored = %d, skipped = %d; want 7 restored, 0 skipped",
			len(restored.Restored), len(restored.Skipped))
	}

	// allowReplace=false skips every existing destination.
	second, err := s.RestoreBackup(snap.ID, false)
	if err != nil {
		t.Fatalf("RestoreBackup(false): %v", err)
	}
	if len(second.Restored) != 0 || len(second.Skipped) != 7 {
		t.Errorf("restored = %d, skipped = %d; want 0 restored, 7 skipped",
			len(second.Restored), len(second.Skipped))
	}
}

// TestServiceExportCollectionJSON exports the on-disk scan as a JSON
// manifest with the expected name, game version and folder list.
func TestServiceExportCollectionJSON(t *testing.T) {
	s, _ := newTestService(t)
	out := filepath.Join(t.TempDir(), "export.json")
	res, err := s.ExportCollection(out, "", false)
	if err != nil {
		t.Fatalf("ExportCollection(json): %v", err)
	}
	if res.Out != out {
		t.Errorf("out = %q, want %q", res.Out, out)
	}
	if res.Addons != 7 {
		t.Errorf("addons = %d, want 7 (fixture folders)", res.Addons)
	}
	if res.Collection != "" {
		t.Errorf("collection = %q, want empty", res.Collection)
	}

	mf, err := importexport.ImportManifest(out)
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	if mf.Name != "wowfix-export" {
		t.Errorf("manifest name = %q, want wowfix-export", mf.Name)
	}
	if mf.GameVersion != "wrath" {
		t.Errorf("game_version = %q, want wrath (cfg.Profile)", mf.GameVersion)
	}
	if len(mf.Addons) != 7 {
		t.Fatalf("manifest addons = %d, want 7", len(mf.Addons))
	}
	folders := map[string]bool{}
	for _, a := range mf.Addons {
		if a.Provider != "" {
			t.Errorf("untracked export entry %q has provider %q", a.Folder, a.Provider)
		}
		folders[a.Folder] = true
	}
	for _, f := range []string{"AtlasLoot", "Questie-main", "DPSMate-main", "AuxUI", "Questie", "Inventory", "TempFolder"} {
		if !folders[f] {
			t.Errorf("manifest missing folder %q", f)
		}
	}
}

// TestServiceExportCollectionByID exports a named collection: the
// manifest carries the collection name and its addon state.
func TestServiceExportCollectionByID(t *testing.T) {
	s, _ := collectionTestService(t, "A", "A.disabled")
	created, err := s.CreateCollection("pve")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	out := filepath.Join(t.TempDir(), "export.json")
	res, err := s.ExportCollection(out, created.ID, false)
	if err != nil {
		t.Fatalf("ExportCollection by id: %v", err)
	}
	if res.Addons != 2 {
		t.Errorf("addons = %d, want 2 (A + A.disabled)", res.Addons)
	}
	mf, err := importexport.ImportManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	if mf.Name != "pve" {
		t.Errorf("manifest name = %q, want pve", mf.Name)
	}
	if len(mf.Addons) != 2 {
		t.Errorf("manifest addons = %d, want 2", len(mf.Addons))
	}
}

// TestServiceExportCollectionYAML writes a YAML manifest that
// ImportManifestAny can read back.
func TestServiceExportCollectionYAML(t *testing.T) {
	s, _ := newTestService(t)
	out := filepath.Join(t.TempDir(), "export.yaml")
	if _, err := s.ExportCollection(out, "", false); err != nil {
		t.Fatalf("ExportCollection(yaml): %v", err)
	}
	mf, err := importexport.ImportManifestAny(out)
	if err != nil {
		t.Fatalf("ImportManifestAny: %v", err)
	}
	if len(mf.Addons) != 7 {
		t.Errorf("manifest addons = %d, want 7", len(mf.Addons))
	}
}

// zipEntries lists the entry names of a zip archive.
func zipEntries(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

// TestServiceExportCollectionZip bundles the fixture and, only when
// includeSavedVars, the first account's SavedVariables tree.
func TestServiceExportCollectionZip(t *testing.T) {
	s, _ := savedVarsTestService(t)
	out := filepath.Join(t.TempDir(), "bundle.zip")
	if _, err := s.ExportCollection(out, "", true); err != nil {
		t.Fatalf("ExportCollection(zip, savedvars): %v", err)
	}
	entries := zipEntries(t, out)
	hasSavedVars := false
	for _, e := range entries {
		if strings.HasPrefix(e, "savedvars/") {
			hasSavedVars = true
		}
	}
	if !hasSavedVars {
		t.Errorf("zip has no savedvars/ tree: %v", entries)
	}
	if !slices.Contains(entries, "manifest.json") {
		t.Errorf("zip missing manifest.json: %v", entries)
	}

	out2 := filepath.Join(t.TempDir(), "bundle2.zip")
	if _, err := s.ExportCollection(out2, "", false); err != nil {
		t.Fatalf("ExportCollection(zip, no savedvars): %v", err)
	}
	for _, e := range zipEntries(t, out2) {
		if strings.HasPrefix(e, "savedvars/") {
			t.Errorf("zip without includeSavedVars contains %q", e)
		}
	}
}

// TestServiceImportCollectionManifest installs the remote entries of a
// JSON manifest through the catalog and skips payload-less local ones.
func TestServiceImportCollectionManifest(t *testing.T) {
	s, addonsDir, _, mock := newTestCatalogService(t)
	mock.repos["acme/newaddon"] = "v1.0.0"
	mock.zips["acme/newaddon"] = addonZipBytes(t, "NewAddon", "## Title: NewAddon\n## Version: 1.0.0\n## Interface: 30300\n")

	mf := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(mf, []byte(`{
		"version": 1, "name": "pve", "game_version": "wrath",
		"addons": [
			{"folder": "NewAddon", "provider": "github", "id": "acme/newaddon", "source": "acme/newaddon"},
			{"folder": "LocalOnly"}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.ImportCollection(mf)
	if err != nil {
		t.Fatalf("ImportCollection(manifest): %v", err)
	}
	if !slices.Contains(res.Installed, "NewAddon") {
		t.Errorf("installed = %v, want NewAddon", res.Installed)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "NewAddon")); err != nil {
		t.Errorf("NewAddon not installed: %v", err)
	}
	if len(res.Installed) != 1 {
		t.Errorf("installed = %v, want exactly NewAddon (local-only skipped)", res.Installed)
	}
}

// TestServiceImportCollectionZip installs a local-only bundle zip:
// the payload folder is extracted into the AddOns directory.
func TestServiceImportCollectionZip(t *testing.T) {
	s, addonsDir := newTestService(t)
	zipPath := filepath.Join(t.TempDir(), "bundle.zip")
	writeZip(t, zipPath, map[string]string{
		"manifest.json":                    `{"version":1,"name":"pve","game_version":"wrath","addons":[{"folder":"LocalAddon"}]}`,
		"addons/LocalAddon/LocalAddon.toc": "## Title: LocalAddon\n## Version: 1.0.0\n",
	})

	res, err := s.ImportCollection(zipPath)
	if err != nil {
		t.Fatalf("ImportCollection(zip): %v", err)
	}
	if !slices.Contains(res.Installed, "LocalAddon") {
		t.Errorf("installed = %v, want LocalAddon", res.Installed)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "LocalAddon")); err != nil {
		t.Errorf("LocalAddon not extracted: %v", err)
	}
}

// TestServiceImportCollectionURL fetches a GitHub repo list and
// installs every entry through the mock provider.
func TestServiceImportCollectionURL(t *testing.T) {
	s, addonsDir, _, mock := newTestCatalogService(t)
	mock.repos["acme/newaddon"] = "v1.0.0"
	mock.zips["acme/newaddon"] = addonZipBytes(t, "NewAddon", "## Title: NewAddon\n## Version: 1.0.0\n## Interface: 30300\n")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "acme/newaddon\n# comment\n\n")
	}))
	defer ts.Close()

	res, err := s.ImportCollection(ts.URL)
	if err != nil {
		t.Fatalf("ImportCollection(url): %v", err)
	}
	if !slices.Contains(res.Installed, "NewAddon") {
		t.Errorf("installed = %v, want NewAddon", res.Installed)
	}
	if _, err := os.Stat(filepath.Join(addonsDir, "NewAddon")); err != nil {
		t.Errorf("NewAddon not installed from list: %v", err)
	}
}

// TestServiceImportCollectionUnsupported rejects arguments that are
// neither an existing manifest/bundle nor an http(s) URL.
func TestServiceImportCollectionUnsupported(t *testing.T) {
	s, _ := newTestService(t)
	_, err := s.ImportCollection(filepath.Join(t.TempDir(), "list.txt"))
	if err == nil {
		t.Fatal("unsupported import argument should error")
	}
}

// TestServiceConfigRoundTrip maps every persisted field into the view.
func TestServiceConfigRoundTrip(t *testing.T) {
	s, addonsDir := newTestService(t)
	root := filepath.Dir(filepath.Dir(addonsDir))
	cv, err := s.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cv.WoWPath != root {
		t.Errorf("wow_path = %q, want %q", cv.WoWPath, root)
	}
	if cv.Profile != "wrath" || cv.Theme != "dark" {
		t.Errorf("profile/theme = %q/%q, want wrath/dark", cv.Profile, cv.Theme)
	}
	if !cv.AutoBackup || !cv.Confirmations {
		t.Errorf("auto_backup/confirmations = %v/%v, want true (config defaults)", cv.AutoBackup, cv.Confirmations)
	}
}

// TestServiceSetConfigKey persists every supported key, including the
// auto_backup alias, and wow_path validates the install path.
func TestServiceSetConfigKey(t *testing.T) {
	s, addonsDir := newTestService(t)
	root := filepath.Dir(filepath.Dir(addonsDir))

	cases := []struct{ key, value string }{
		{"flavor", "classic"},
		{"theme", "light"},
		{"profile", "tbc"},
		{"autobackup", "true"},
		{"auto_backup", "false"},
		{"confirmations", "true"},
		{"backups_dir", filepath.Join(t.TempDir(), "backups")},
		{"curseforge_api_key", "abc123"},
		{"collection", "pve"},
		{"collections_dir", filepath.Join(t.TempDir(), "cols")},
		{"wow_path", root},
	}
	for _, c := range cases {
		if err := s.SetConfigKey(c.key, c.value); err != nil {
			t.Fatalf("SetConfigKey(%q, %q): %v", c.key, c.value, err)
		}
	}

	cv, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cv.WoWPath != root || cv.Flavor != "classic" || cv.Profile != "tbc" ||
		cv.Theme != "light" || cv.AutoBackup || !cv.Confirmations ||
		cv.BackupsDir == "" || cv.CurseForgeAPIKey != "abc123" ||
		cv.Collection != "pve" || cv.CollectionsDir == "" {
		t.Errorf("config view = %+v", cv)
	}
}

// TestServiceSetConfigKeyValidation rejects bad values with the
// expected error texts.
func TestServiceSetConfigKeyValidation(t *testing.T) {
	s, _ := newTestService(t)
	bad := []struct{ key, value, wantErr string }{
		{"theme", "blue", "theme must be dark or light"},
		{"profile", "nope", "unknown profile"},
		{"autobackup", "maybe", "must be true or false"},
		{"auto_backup", "maybe", "must be true or false"},
		{"confirmations", "maybe", "must be true or false"},
		{"wow_path", filepath.Join(t.TempDir(), "missing"), "path does not exist"},
		{"unknown", "x", "unknown key"},
	}
	for _, c := range bad {
		err := s.SetConfigKey(c.key, c.value)
		if err == nil {
			t.Errorf("SetConfigKey(%q, %q) should error", c.key, c.value)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("SetConfigKey(%q, %q) err = %q, want containing %q", c.key, c.value, err.Error(), c.wantErr)
		}
	}
}
