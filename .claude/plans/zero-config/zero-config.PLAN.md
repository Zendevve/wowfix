# wowfix zero-config — PLAN

## Goal
First-run and repair flows must be toddler-simple and zero-config: open the app → wowfix finds the game, scans, and offers ONE button to repair everything. No wizard, no game-version questions, no confirmation dialogs on reversible repairs. Settings internals move behind "Advanced".

## Context (verified from source)
- Backend `resolveInstall` (internal/service/service.go) ALREADY falls back to `detector.AutoDetect` and picks the best install when `cfg.WoWPath == ""`. Detection derives flavor + profile from the client exe (PE version). The backend is already zero-config-capable.
- The frontend blocks it: `GetState` (service.go ~line 1022) sets `HasInstall=false` whenever `cfg.WoWPath == ""`, so `main.ts` boot routes to the mandatory `setup` wizard even when installs were detected.
- Setup wizard (frontend/src/views/setup.ts): detected cards + manual form + flavor select + game-version (profile) select + "Continue to scan" CTA.
- Overview (frontend/src/views/overview.ts): Fix All is gated behind a `confirmDialog`; individual row fixes already skip it (`service.Fix(folder, true)`).
- Settings (frontend/src/views/settings.ts): raw config keys editable inline (wow_path, flavor, profile, collection, backups_dir, curseforge_api_key, collections_dir) + a multi-install grid (InstallsStatus, SyncUpdatesToAll).
- Statusbar (main.ts `renderStatus`): shows the profile name (jargon for toddlers).
- Mock mode: `?mock=1` installs a Proxy service (frontend/src/mock/index.ts; GetState default `has_install: true`); each view has `mock/<view>.ts` exporting `data`.

## Design decisions (settled — implement to spec, do not redesign)
1. **Auto-adopt on first run.** When `cfg.WoWPath == ""` and auto-detect finds ≥1 install, `GetState` persists the best install (wow_path, flavor, and profile from `install.ProfileID` when non-empty) and returns `HasInstall=true`. The setup wizard then appears only when nothing is detectable or the saved path is stale. Multi-install users land on the best-confidence install and can switch in Settings (Advanced).
2. **Setup view = minimal fallback.** Single path field + Browse + CTA ("Start using wowfix"). Remove flavor select, profile select, detected-cards, and the auto-detect call. Flavor derives from the typed path (keep `deriveFlavorFromPath`); profile comes from detection at `SetInstall` time. Keep stale-path prefill.
3. **One-click repair.** Overview `fixAll` drops the `confirmDialog` (repairs are fully reversible: auto_backup snapshots + OS trash). Put the trust copy inline near the CTA: "Backed up first · removals go to the OS trash". Confirmations remain on genuinely destructive ops elsewhere (restore-overwrite, SavedVariables reset, collection delete) — those views are untouched by this plan.
4. **Settings split.** Main view: active-install card (root/flavor/profile read-only) + "Change install" (native folder picker → `SetInstall` + `SetProfile` from the returned install) + behavior toggles (auto_backup, confirmations) + About. All raw config-key fields and the installs multi-grid move under an `<details>` "Advanced" disclosure. All existing backend calls stay reachable (SetConfigKey, InstallsStatus, SyncUpdatesToAll, per-install Scan).
5. **Statusbar**: replace the profile-name chip with the flavor label (Retail / Wrath Classic / Classic Era / TBC Classic / Top-level); drop the chip when flavor is unknown. Label source: `app.state.flavor` mapped like setup's FLAVOR_LABEL.

## Slice contracts (one agent per slice; NO file overlaps)
| Slice | Files (only these) | Key contract |
|---|---|---|
| A backend | internal/service/service.go, internal/service/service_test.go | GetState auto-adopt + persist; new test `TestGetStateAutoAdopts`; keep `TestGetStateStalePath` / `TestGetStateValidPath` green. Add `autoDetect func(context.Context) ([]detector.Installation, error)` field seam on Service (default `detector.AutoDetect`) following the existing private-seam pattern (see comment at service_test.go:1009). `SetInstall`/`SetProfile` signatures unchanged. |
| B shell+setup | frontend/src/views/setup.ts, frontend/src/main.ts, frontend/src/views/setup.css (trim dead styles only), frontend/src/mock/setup.ts | setup.ts minimal fallback; main.ts statusbar flavor chip; mock parity with the new minimal view. |
| C overview | frontend/src/views/overview.ts, frontend/src/views/overview.css (only if needed) | Fix All without dialog + inline safety copy. |
| D settings | frontend/src/views/settings.ts, frontend/src/views/settings.css | Advanced disclosure; active-install card; keep all service calls. |
| E docs | README.md, CHANGELOG.md | Zero-config first run, one-click repair, Advanced settings; CHANGELOG entry "v3.1.0 — zero-config first run" style matching existing entries. |

## Conventions
- Repo root: `D:/COMPROG/wow addon compendium/wowfix` — all paths below are relative to it. Run shell commands with that cwd.
- Design language: DESIGN.md tokens; this is NOT a restyle — minimal markup changes. Reuse existing component classes (btn-primary, btn-secondary, text-input, card, spotlight-card, …) and the existing per-view CSS; do not invent new visual language.
- Views are vanilla TS string templates, no framework. Keep each view's existing `disposed` guard and render-loop pattern. Mock parity per view (`mock/<view>.ts` exports `data`).
- Skip project-wide validation mid-flight (whole-project `tsc`/vite build may false-fail on peers' half-done files). Agent A MAY run `go test ./...` + `go vet ./...` (only agent touching Go). The orchestrator runs final gates after the batch.
- Go: go.mod pins 1.25.0; local go1.23.4 with GOTOOLCHAIN=auto → the first `go` command auto-downloads the toolchain (needs network).
- Do not edit CHANGELOG.md release history beyond appending the new entry. Do not touch DESIGN.md, LEARNINGS.md, or the screenshots.

## Acceptance (final gates, run by orchestrator after the batch)
- `cd frontend && npm run build` (tsc --noEmit + vite build) passes.
- `go test ./...` and `go vet ./...` pass.
- Mock browser smoke: default boot lands on overview with a scan; `?mock=1&view=setup` renders the minimal fallback; overview Fix All acts without a dialog; settings shows the install card + Advanced disclosure.
