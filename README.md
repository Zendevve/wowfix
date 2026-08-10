# wowfix

wowfix scans your World of Warcraft `Interface/AddOns` folder, finds the common addon installation problems, repairs them safely (with backups and trash), validates TOC compatibility and installs addons from ZIP archives. It ships as a Windows desktop GUI built with Wails v2 (WebView2). The CLI was removed in v3.

![Go](https://img.shields.io/badge/Go-1.25+-00ADD8) ![Platform](https://img.shields.io/badge/Windows-lightgrey) ![CI](https://img.shields.io/github/actions/workflow/status/Zendevve/wowfix/ci.yml?branch=main) ![Release](https://img.shields.io/github/v/release/Zendevve/wowfix)

---

## Screenshots

The desktop GUI in mock mode (1440×900), showing the eight destinations:

**Setup** — zero-config first run: wowfix finds your install, scans and repairs with one click; this fallback screen appears only when no install can be found automatically:
![Setup](screenshots/setup.png)

**Overview** — segmented health workflows: Scan, Doctor, Validation:
![Overview](screenshots/overview.png)

**Catalog** — five-provider search, curated band, ZIP browse, Wago imports:
![Catalog](screenshots/catalog.png)

**Updates** — bulk check, review list, per-addon apply with pin/ignore/rollback:
![Updates](screenshots/updates.png)

**Collections** — named addon loadouts with enable toggles:
![Collections](screenshots/collections.png)

**Backups** — snapshot list with offline export/check:
![Backups](screenshots/backups.png)

**SavedVariables** — per-account backup, restore, reset, migrate:
![SavedVariables](screenshots/savedvars.png)

**Settings** — behavior, paths, API key, per-install cards:
![Settings](screenshots/settings.png)

**Design language** — the desktop GUI is dark-only, drawn on a near-black canvas (`#0b0b0a`) with warm charcoal surfaces. Primary actions are white pills with black text; secondary controls are charcoal pills; accent blue (`#0099ff`) is reserved for links, focus rings and selection states. Display type is Mona Sans Variable with tight negative letter-spacing; body copy is Inter Variable with OpenType variants (`cv01, cv05, cv09, cv11, ss03, ss07, dlig`). Gradient spotlight cards (violet, magenta, orange, coral) are used sparingly — setup screen and catalog's curated band.

**Regenerating screenshots** — run `cd frontend && npm install && npm run dev`, open `http://localhost:5173/?mock=1&view=<view>` (`?mock=1&view=setup` for first-run) and capture at ~1440×900. Valid views: `setup`, `overview`, `scan`, `doctor`, `validate`, `catalog`, `updates`, `collections`, `backups`, `savedvars`, `settings`.

---

## Features

### Repair engine
- **Scan & detect** — every folder in `Interface/AddOns` is analyzed for:
  1. **GitHub folder names** — `Questie-main` → rename to `Questie` (`-main`, `-master`, `-dev`, version suffixes)
  2. **TOC name mismatch** — `Aux/` containing `Aux-Classic.toc` → rename folder to `Aux-Classic`
  3. **Nested folders** — `DPSMate-main/DPSMate/DPSMate.toc` → flatten to top level
  4. **Missing TOC** — folder without any `*.toc` → marked invalid, move to trash offered
  5. **Multiple TOCs** — `Atlas.toc` + `Atlas_Wrath.toc` + `Atlas_TBC.toc` → user picks the defining TOC
  6. **Empty folders** — detected, optionally trashed
  7. **Duplicate addons** — `Questie/` + `Questie-main/` → merge or delete
  8. **Broken extraction structure** — several addons dumped into one folder → promoted to top level
- **Fix all** — one-click repair of every detected problem, with no confirmation dialog on reversible repairs: each change is backed up first, removals go to the OS trash.

### TOC validation
- Parses every TOC, reports `Expected interface / Detected interface / Status` against any of 9 profiles: Vanilla 1.12, TurtleWoW, TBC 2.4.3, WotLK 3.3.5a, Cataclysm, Classic Era, Hardcore, Season of Discovery, Retail. TOCs are never edited.

### Catalog & install
- **Five providers** — GitHub, CurseForge, WoWInterface, Tukui, Wago (WeakAuras/Plater import strings) searched in parallel with merged results.
- **Single install surface** — search, URL/owner-repo install bar, **Browse…** for local ZIPs, drag-drop ZIPs onto the bar.
- **Curated private-server sets** — hand-verified manifest of known-good addons for vanilla-family clones (Turtle-style 1.12) and ChromieCraft (WotLK 3.3.5a), each anchored to a GitHub source.
- **Provider filters, info panel, Wago import** — all in the Catalog view.

### Updates
- **Bulk pre-check, then cache-only loop** — one provider call per batch populates caches; the per-addon loop never re-hits the network.
- **Review list** — check-all → review list (old → new version, in-app changelog) → apply.
- **Per-addon controls** — Update, Pin (lock at current version), Ignore (exclude from management), History (version log with per-row rollback), Rollback (re-downloads exact version from provider).
- **Honest failure surfacing** — a failed addon is data: row-level error state, never a batch abort, never a blanket toast.
- **Provider outage surfacing** — catalog shows per-provider status with honest caveats, never silent suppression.

### Collections (addon profiles)
- Named addon loadouts (PvE/PvP/Raiding/Leveling presets) with enable toggles.
- Switching renames folders between `<name>` and `<name>.disabled` — nothing is deleted; every switch is preceded by a backup snapshot.
- Duplicate, rename, delete, per-addon toggle.

### SavedVariables
- Per-account backup, restore, reset, and migrate between accounts under `WTF/Account/<account>/SavedVariables`.
- Listing an account's files automatically creates one backup per account per session; manual backup always works.

### Backups & snapshots
- Every mutation is preceded by a `Backups/<timestamp>/` snapshot; `restore` brings folders back (current state snapshotted first).
- **Offline catalog snapshots** — export freezes tracked addons with latest known versions into portable JSON while online; check diffs it against the registry with no network.

### Safety model
1. **No prompts for reversible repairs** — the GUI does not ask for confirmation on reversible repairs (Fix all, per-addon fixes): every change is backed up first and removals go to the OS trash, which makes them safe to apply unprompted. Confirmation dialogs remain for genuinely destructive operations — restore-overwrite, SavedVariables reset, collection delete.
2. **Always back up first** — each affected folder is copied to `Backups/<timestamp>/` before any change; disabled only via `auto_backup: false` config.
3. **Never delete permanently** — removal means moving to the OS trash (Recycle Bin on Windows); if native trash fails (e.g. cross-device), a copy is kept in the fallback trash directory and the source is removed.
4. **Permission errors are graceful** — unreadable folders are reported per-addon and never abort a scan; unwritable destinations fail with a clear message.

### Import / Export
- **Manifest** — JSON or YAML describing an addon setup (folder, provider, source, version).
- **Bundle ZIP** — manifest + local addon folders + SavedVariables; restores into first account on import.
- **GitHub repo list** — one `owner/repo` per line, `#` = comment.

### Install detection
- Finds WoW installs in standard locations, Battle.net/Steam registry keys (Windows), Wine/Lutris/Proton prefixes (Linux), Applications (macOS).
- Game version read from the client executable's PE version resource.
- Private-server client detection: folder accepted when it contains a known client executable (`wow.exe`, `WowClassic_TBC.exe`, `wow-64.exe`, …) or an `Interface` folder, even when `Interface/AddOns` does not exist yet (created on first scan).

---

## Install / Build

Requires Go 1.25+ (the `go` directive in `go.mod` pins the toolchain; `go build` auto-installs the correct version). Node.js 18+ and npm are needed to build the GUI frontend. WebView2 runtime is required on Windows (pre-installed on Windows 10 1809+ / Windows 11).

**Desktop GUI (Windows, WebView2 runtime required):**

```sh
wails build                         # -> build/bin/wowfix.exe
wails build -nsis                   # + build/bin/wowfix-setup.exe installer
```

The CI workflow (`.github/workflows/release.yml`) builds both artifacts on every tag.

---

## Development + Mock Workflow

```sh
cd frontend
npm install
npm run dev           # -> http://localhost:5173
```

Open `http://localhost:5173/?mock=1&view=<view>` to render any destination with seeded mock data (no Go backend required). Valid views:

- `setup` — first-run install picker
- `overview` — segmented Scan/Doctor/Validation
- `scan` — scan list with issues
- `doctor` — environment diagnostics
- `validate` — TOC compatibility table
- `catalog` — provider search + curated band
- `updates` — tracked addons with pin/ignore/rollback
- `collections` — named loadouts with toggles
- `backups` — snapshot list + offline export/check
- `savedvars` — per-account SavedVariables
- `settings` — behavior, paths, API key, installations

---

## Configuration

Stored at `os.UserConfigDir()/wowfix/config.json`:

On first run, `wow_path` and `flavor` are set automatically from the detected WoW install — no configuration needed; the game `profile` follows when the detected client identifies a version. The Settings view leads with the active install and behavior toggles; the raw config keys live under **Advanced**.

- `wow_path` — WoW installation root
- `flavor` — client subfolder (`_retail_`, `_classic_`, `_classic_era_`, `_classic_tbc_`, or root)
- `profile` — one of `vanilla, turtle, tbc, wrath, cata, classic, hardcore, sod, retail`
- `auto_backup` — snapshot before every mutation (default `true`)
- `confirmations` — confirm destructive actions (default `true`)
- `backups_dir` — override the `Backups/` location (default: next to the game)
- `curseforge_api_key` — enables the modern CurseForge Core API (without it the catalog falls back to the deprecated legacy endpoint); the `WOWFIX_CURSEFORGE_API_KEY` environment variable takes precedence
- `collection` — the active addon-collection id (set by Collections view)
- `collections_dir` — where collection files live (default: `<config dir>/collections`)

The GUI has a Settings view for all keys (raw keys under **Advanced**); the config file can also be edited manually.

---

## Safety model

See [Features → Safety model](#safety-model) above. In short: backup → trash, never permanent delete, graceful permission handling; reversible repairs never prompt for confirmation.

---

## Design system

See [DESIGN.md](DESIGN.md) for the complete Framer design language tokens (colors, typography, spacing, elevation, components) and app-adaptation rules. The frontend is vanilla TypeScript + Vite with no UI framework.

---

## Learnings

See [docs/LEARNINGS.md](docs/LEARNINGS.md) for the distilled 30-project competitor study: what the field taught us, which adoptions land in v3, the Go core roadmap (R1–R15), and the consolidated anti-pattern list. The competitor corpus lives in `refs/competitors/` (gitignored); its synthesis is `refs/competitors/study/README.md`.

---

## Architecture

```
main.go                 Wails v2 entry point (binds Go service to frontend)
internal/
  service/              Wails-bound API facade (scan/fix/validate/install/catalog/updates/collections/backups/savedvars/config)
  gui/                  Wails application wiring (options, bindings, menu, runtime)
  models/               shared data types: Addon, TOC, Issue, Profile, RegistryEntry, Collection, BackupManifest
  catalog/              providers (GitHub/CurseForge/WoWInterface/Tukui/Wago), registry, updater, semver
  scanner/              detection only — never touches the filesystem
  validator/            TOC parser + compatibility classification (9 profiles)
  fixer/                repairs: rename, flatten, merge, delete (with backups)
  installer/            ZIP extraction, normalization, install, validate
  backup/               timestamped snapshots + manifest + restore
  profiles/             addon collections: capture, apply (.disabled renames)
  savedvars/            SavedVariables backup/restore/reset/migrate under WTF/Account
  importexport/         manifest/bundle/GitHub-list export & import
  detector/             WoW install discovery + PE version parsing
  config/               persisted user configuration
  logger/               ring-buffer logger with file sink + export
  utils/                filesystem helpers, cross-platform trash, PE parser
```

The core packages (scanner, validator, fixer, installer, backup, catalog, config, profiles, savedvars, importexport) are pure business logic with no UI dependency.

---

## Testing

```sh
go test ./...
go test ./internal/e2e/ -count=1 -v   # end-to-end pipeline: scan -> fix -> backup/restore -> install
go vet ./...
```

The scanner and validator have unit tests covering every detection rule and every compatibility classification. The `internal/e2e` test drives the whole pipeline against a fake `Interface/AddOns` tree in a temp dir: it scans the fixture, fixes every problem (with backups and trash), restores a corrupted tree from a snapshot and installs addons from a ZIP archive.

**CI** — a GitHub Actions workflow (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, tests and the GUI build on Windows.

---

## Project history

- **v1.0.0** (2026-08-05) — initial release: repair engine, TOC validation, catalog, updates, collections, SavedVariables, import/export, backups, safety model, Bubble Tea TUI, full CLI.
- **v2.x** (2026-08-07/08) — Wails v2 desktop GUI added alongside CLI; Framer design language; sidebar navigation; CLI↔GUI parity.
- **v3.0.0** (2026-08-08) — **ground-up GUI-only rebuild**: CLI removed; frontend rewritten from scratch (vanilla TS + Vite) per Framer design language; 8 destinations; mock mode; 30-project competitor study → `docs/LEARNINGS.md`; v2 archived on branch `archive/v2-legacy` and zip in parent `archive/` directory.

---

## License

Proprietary — see [LICENSE](LICENSE). Copyright (c) 2026 Zendevve. All rights reserved. Personal, non-commercial use within World of Warcraft is permitted; modification, redistribution, and reuse of the source code require prior written permission from the copyright holder.