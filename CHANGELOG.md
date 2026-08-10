# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [3.1.0] - 2026-08-10

Zero-config first run: wowfix auto-adopts the detected WoW install, scans and repairs with one click — the setup wizard is now just a fallback.

### Added

- **Zero-config first run** — when no install is configured, wowfix auto-detects the WoW installation on launch and adopts it: `wow_path` and `flavor` are set from the detected client (the game `profile` follows when the detected client identifies a version), and the app lands on Overview with a scan ready. No wizard, no game-version questions.
- **One-click Fix All** — the Overview "Fix all" action repairs every detected problem in a single click; reversible repairs never prompt (each change is backed up first, removals go to the OS trash).
- **Settings "Advanced" section** — the Settings view leads with the active-install card (root/flavor/profile read-only), "Change install", behavior toggles and About; the raw config-key fields and per-install grid moved under an "Advanced" disclosure (all existing service calls stay reachable).
- **Statusbar flavor label** — the statusbar now shows the client flavor (Retail / Wrath Classic / Classic Era / TBC Classic / Top-level) instead of the profile name; the chip is dropped when the flavor is unknown.

### Changed

- **Setup wizard reduced to a fallback** — the wizard now appears only when no install can be found automatically or the saved path is stale: a single path field + Browse + "Start using wowfix". Flavor derives from the typed path; the profile comes from detection at install time.
- **Safety model** — the GUI no longer prompts for reversible repairs (auto-backup + OS trash make them safe); confirmation dialogs remain for genuinely destructive operations: restore-overwrite, SavedVariables reset and collection delete.

## [3.0.0] - 2026-08-08

### Added

- **Ground-up GUI-only rebuild** — the CLI (`cmd/wowfix`) is removed; wowfix is now a Wails v2 desktop app only.
- **Frontend rewritten from scratch** — vanilla TypeScript + Vite, implementing the Framer design language per `DESIGN.md` (near-black canvas, white-pill CTAs, accent blue for links/focus only, gradient spotlight cards, Inter Variable with OpenType variants, Mona Sans Variable for display).
- **Eight destinations** — Setup, Overview (Scan/Doctor/Validation segmented), Catalog, Updates, Collections, Backups, SavedVariables, Settings.
- **Mock mode for dev/screenshots** — `?mock=1&view=<view>` in Vite dev server seeds every destination with realistic data (no Go backend required).
- **30-project competitor study distilled** — `docs/LEARNINGS.md` captures field analysis, adoptions for v3 (R1–R15 roadmap), and consolidated anti-patterns; corpus in `refs/competitors/` (gitignored).

### Changed

- **Repair engine** — scan/fix/repair engine carried from v2: GitHub folder names, TOC mismatches, nested folders, missing/multiple TOCs, empty folders, duplicates, broken extraction structures; fix-all with backups and OS-trash removal.
- **TOC validation** — 9 profiles (Vanilla 1.12, TurtleWoW, TBC 2.4.3, WotLK 3.3.5a, Cataclysm, Classic Era, Hardcore, Season of Discovery, Retail) carried unchanged.
- **Catalog & install** — 5 providers (GitHub, CurseForge, WoWInterface, Tukui, Wago) with parallel search, merged results, ZIP browse/drag-drop, curated private-server sets (vanilla-family, ChromieCraft), Wago WeakAuras/Plater imports.
- **Updates** — bulk pre-check → cache-only loop; review list with old→new versions and in-app changelog; per-addon Pin/Ignore/History/Rollback; honest per-addon failure surfacing; provider outage surfacing (never silent suppression).
- **Collections** — named addon loadouts with enable toggles; `.disabled` rename switching; duplicate/rename/delete; backup-before-switch.
- **SavedVariables** — per-account backup/restore/reset/migrate; auto-backup on first list; manual backup always available.
- **Backups & snapshots** — timestamped `Backups/<ts>/` before every mutation; restore snapshots current state first; offline catalog snapshot export/check.
- **Safety model** — confirm → backup → trash (never permanent delete); graceful permission errors.
- **Import/Export** — JSON/YAML manifests, bundle ZIPs (manifest + local addons + SavedVariables), GitHub repo lists.
- **Install detection** — Retail/Classic/etc. auto-detection; private-server client detection (known exe or Interface folder); PE version parsing.
- **Config** — `wow_path`, `flavor`, `profile`, `auto_backup`, `confirmations`, `backups_dir`, `curseforge_api_key`, `collection`, `collections_dir` (theme removed; dark-only).

### Removed

- **CLI** — `cmd/wowfix` deleted; all CLI commands (`scan`, `fix`, `install`, `validate`, `list`, `search`, `update`, `history`, `rollback`, `snapshot`, `sources`, `curated`, `backup`, `restore`, `doctor`, `config`, `profile`, `savedvars`, `export`, `import`, `version`) removed.
- **Cross-compile targets** — Linux/macOS CLI builds dropped (Windows GUI only; cross-platform capable via Wails but not shipped).
- **Terminal UI references** — Bubble Tea TUI was already removed in v2.0.0.
- **`theme` config key** — UI is dark-only; key no longer stored or read.

### Archived

- **v2.3.0** tagged and archived on branch `archive/v2-legacy` with a zip in the parent `archive/` directory.
## [2.3.0] - 2026-08-08

### Added

- **Command palette** — press Ctrl+K / Ctrl+P anywhere to fuzzy-search
  views and actions and jump straight to them.
- **Per-addon version history and rollback** — the Updates view row menu
  shows the recorded version log of a tracked addon and re-downloads a
  specific past version. New CLI commands: `wowfix history <folder>` and
  `wowfix rollback <folder> <version>`.
- **SavedVars auto-backup** — Saved Variables are backed up automatically
  the first time the list is opened.
- **Setup flavor detection** — the setup flow now auto-derives the client
  flavor (retail/classic) from the chosen install path.

### Changed

- **GUI overhaul — six-destination sidebar** — navigation is regrouped
  into a compact sidebar: **Overview** hosts the Scan/Doctor/Validation
  segments; **Catalog** is the single install surface, with ZIP browse
  and drag-drop; **Backups** hosts the offline snapshot export/check;
  **Settings** shows per-install status; **SavedVars** gains an Advanced
  operations disclosure.
- **Accessibility / reduced-motion hardening** — focus management,
  contrast and animation handling tightened for keyboard and
  reduced-motion users.

## [2.2.0] - 2026-08-07

### Added

- **CLI→GUI parity** — every CLI command now has a working GUI
  equivalent. New service methods: `AddonInfo`, `Sources`, `Doctor`,
  `SavedVarsAccounts/List/Backup/Restore/Reset/Migrate`, `BackupNow`,
  `ListBackups`, `RestoreBackup`, `ExportCollection`, `ImportCollection`,
  `Config`, `SetConfigKey` (theme, auto-backup, confirmations, backups
  dir, CurseForge API key, collections dir). Five new views — Doctor,
  Saved Variables, Backups, Settings, Export/Import; the Updates view
  gains offline snapshot export/check; the Catalog view gains addon
  details and provider sources. `docs/GUI_PARITY.md` maps every CLI
  command to its GUI feature with live-verification evidence.
- **Sidebar navigation** — the 12-item tab bar is replaced by a grouped
  left sidebar (Overview / Maintenance / Data / System) with a
  collapsible icon rail and arrow-key navigation.
- **View-switch transition** — a short fade-and-rise entry animation
  that respects `prefers-reduced-motion`.

### Changed

- **Frontend restyle — Framer design language** — the GUI now uses a
  near-black canvas (`#0b0b0a`) with warm charcoal surfaces, a white-pill
  CTA system and accent blue reserved for links, focus rings and
  selection states; Inter Variable + Mona Sans Variable are bundled via
  `@fontsource`. `frontend/src/style.css` is a module entry importing
  tokens/base/components/shell/setup/scan/lists/catalog, and gradient
  spotlight cards anchor the setup brand panel and the catalog's curated
- **Header declutter** — the brand moved into the sidebar; the header is
  now a slim context bar with the install path, profile select and Scan.

### Fixed

- **UX audit fix pass** — modal focus containment + restore + cancel-first on danger dialogs; focus retention across re-renders; visible input focus rings (WCAG 2.4.7/2.4.11); readable metadata text (ink-faint promoted, WCAG 1.4.3); visible form boundaries (1.4.11); ≥24px targets (2.5.8); header no longer clips at min window; addon rows fixed ARIA (4.1.2); aria-busy on loading regions; catalog search clear button + install-bar Escape; mock updates seed.

## [2.1.0] - 2026-08-07

### Added

- **Updates view** — check for updates, dry-run update-all, per-addon
  apply and flavor-mismatch gating.
- **Catalog view** — five providers including WeakAuras/Wago imports and
  paste-URL install.
- **Collections view** — create, switch, detail and per-addon toggles.
- **Installs view** — per-install health cards and cross-install
  update-all.
- **Addon Doctor** — health-score scan view with a fix-all diff toast.
- **Managed addons** — pin/ignore/rollback for tracked addons, integrity
  drift badges and restore-from-source.
- **Recommended section** — curated private-server addon sets (vanilla
  clones, ChromieCraft 3.3.5a) with a flavor-compat filter.
- **Wago provider** — WeakAuras search and imports in the catalog; addon
  manifest checksums.
- **Offline catalog snapshots** — `wowfix snapshot export|check`; backup
  per-folder rollback.
- **CLI** — `wowfix curated list|install`.

### Changed

- **Go 1.25+ required (wails v2.13.0); Node.js needed to build the
  frontend** — the CI test job now builds the frontend before vet/test
  (embed fix) on Go 1.25.x pins.

## [2.0.0] - 2026-08-07

### Added

- **Wails v2 desktop GUI** — a Windows desktop app (scan/fix, TOC
  validation, ZIP install) bound to the same core packages as the CLI;
  the Bubble Tea TUI is removed. Building now requires Go 1.25+ (wails
  v2.13.0) and Node.js for the frontend; the GUI runs on Windows with
  the WebView2 runtime.

### Fixed

- **Manual path entry accepts `Interface\AddOns` paths** — pasting an
  AddOns (or `Interface`) folder into the path prompt now resolves back to
  the game root and flavor instead of failing with `not a directory`. The
  same input works for `wowfix --path`.
- **Clearer path errors** — the scan path error now distinguishes a missing
  path (`path does not exist`), a non-directory (`not a directory`) and a
  root without an AddOns folder, so a bad manual entry is actionable.
- **No more dead-end picker bounce** — when a manually entered or saved
  path fails to scan, the UI returns to the path prompt with the value
  prefilled instead of dropping the user in an empty picker with a second
  `No WoW installation auto-detected` toast.

### Added

- **Private-server client detection** — a folder is accepted as a WoW
  installation when it contains a known client executable (`wow.exe`,
  `WowClassic_TBC.exe`, `wow-64.exe`, …) or an `Interface` folder, even
  when `Interface\AddOns` does not exist yet. The folder is created on the
  first scan so fresh or partial clients work immediately (UI and CLI).
- **Clipboard in text inputs** — `ctrl+v` pastes into the focused input
  (path, filter, catalog search) at the cursor, and `ctrl+y` copies its
  value. `ctrl+c` still quits; terminal-native copy shortcuts keep working.

### Changed

- **TUI visual redesign** — the whole interface now shares one design
  language: a structured header that stays on one line (path middle-
  truncates), a reworked addon list where the problem is always visible
  (status · addon · problem · version · source · fix), two-line
  installation picker, severity-colored toasts, actionable empty states,
  a per-view summary line ("N addons · K with issues · E errors"), a
  two-column help overlay and width-constrained rows in every list
  (catalog, updates, profiles, SavedVariables, logs).

### Removed

- Bubble Tea terminal UI (`internal/ui`) and the `preview` command; the bare
  `wowfix` invocation now prints help and points to the desktop GUI.

## [1.0.0] - 2026-08-05

### Added

- **Repair engine** — scans `Interface/AddOns` and detects the common addon
  installation problems (GitHub-style folder names, TOC name mismatches,
  nested folders, missing TOCs, multiple TOCs, empty folders, duplicates,
  broken extraction structures), then repairs them safely with backups and
  OS-trash removal.
- **TOC validation** — parses every TOC and reports expected/detected
  interface compatibility against nine game profiles (Vanilla 1.12,
  TurtleWoW, TBC 2.4.3, WotLK 3.3.5a, Cataclysm, Classic Era, Hardcore,
  Season of Discovery, Retail).
- **Catalog & providers** — parallel search across GitHub, CurseForge,
  WowInterface and Tukui with merged results; installs addons from ZIP
  archives, provider URLs and `owner/repo` sources; graceful degradation
  when a provider is unreachable.
- **Update manager** — tracks catalog installs in a registry; `wowfix update`
  checks every tracked addon against its provider and applies newer
  releases, flagging game-version mismatches.
- **Addon profiles (collections)** — capture the current addon setup as a
  named collection, switch between collections via `.disabled` folder
  renames, and duplicate/rename/delete them.
- **SavedVariables** — per-account backup, restore, reset and migration of
  `WTF/Account/<account>/SavedVariables` files.
- **Import / export** — share addon setups as JSON/YAML manifests, bundle
  ZIPs (manifest + local addons + SavedVariables) or GitHub repo lists.
- **Terminal UI v2** — Bubble Tea TUI with fuzzy addon filter, help overlay,
  catalog browser, updates panel, collections view, SavedVariables view,
  install-from-source, logs and mouse-wheel scrolling throughout.
- **CLI** — full command set (`scan`, `fix`, `install`, `validate`, `list`,
  `search`, `update`, `backup`, `restore`, `doctor`, `config`, `profile`,
  `savedvars`, `export`, `import`, `preview`, `version`) with `--json`
  machine-readable output.
- **Safety model** — snapshot backup before every mutation, removal via the
  OS trash (never permanent deletion), confirmation prompts for destructive
  actions, and graceful handling of permission errors.
- **CI & e2e** — GitHub Actions workflow (fmt, vet, test, build,
  cross-compile) on Ubuntu and Windows, plus an end-to-end pipeline test
  (scan → fix → backup/restore → install) against a fake addon tree.
- **Release** — `v1.0.0` tag with reproducible versioned builds
  (`-ldflags` metadata) and this changelog.
