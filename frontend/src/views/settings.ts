// Settings view. Main content: the active-install card (read-only root /
// flavor / profile, Change install via native folder picker → SetInstall +
// SetProfile), the behavior toggles (auto_backup, confirmations), and an
// About block (version from GetState). Everything else — the raw config keys
// (inline edit → SetConfigKey) and the installations grid (InstallsStatus +
// SetInstall/SetProfile + Scan + SyncUpdatesToAll) — lives behind the
// "Advanced" disclosure. Busy states disable the acting control only;
// failures surface at the section level.

import type { View } from "../view";
import type {
  ConfigView,
  Install,
  InstallStatus,
  InstallsStatusResult,
  Profile,
  SyncResult,
} from "../types";
import { icon } from "../icons";
import { service } from "../api";
import { toast } from "../toast";
import { confirmDialog } from "../dialog";
import "./settings.css";

/** Editable text keys, in display order. Booleans are rendered as switches. */
const TEXT_KEYS = [
  "wow_path",
  "flavor",
  "profile",
  "collection",
  "backups_dir",
  "curseforge_api_key",
  "collections_dir",
] as const;

const BOOL_KEYS = ["auto_backup", "confirmations"] as const;

const KEY_LABELS: Record<string, string> = {
  wow_path: "WoW install path",
  flavor: "Flavor",
  profile: "Profile",
  collection: "Active collection",
  auto_backup: "Auto-backup",
  confirmations: "Confirmations",
  backups_dir: "Backups folder",
  curseforge_api_key: "CurseForge API key",
  collections_dir: "Collections folder",
};

const KEY_HINTS: Record<string, string> = {
  wow_path: "Normally set from Setup — editing here overrides the current install.",
  flavor: "Game flavor (retail / classic / classic_era).",
  profile: "Normally set from the Installations section.",
  collection: "Normally switched from Collections.",
  auto_backup: "Snapshot addons before every change — fixes, installs, switches.",
  confirmations: "Ask before destructive operations.",
  backups_dir: "Where addon snapshots are stored.",
  curseforge_api_key: "Optional key for higher CurseForge rate limits. Stored locally.",
  collections_dir: "Where exported collection profiles are saved.",
};

const GROUP_ORDER: { title: string; keys: readonly string[] }[] = [
  { title: "Install", keys: ["wow_path", "flavor", "profile", "collection"] },
  { title: "Paths & API key", keys: ["backups_dir", "curseforge_api_key", "collections_dir"] },
];

export const view: View = {
  id: "settings",
  label: "Settings",
  icon: "settings",
  mount(host) {
    let cfg: ConfigView | null = null;
    let installs: InstallsStatusResult | null = null;
    let profiles: Profile[] = [];
    let version = "";
    let cfgError: string | null = null;
    let installsError: string | null = null;
    let loading = true;
    let saving: string | null = null; // config key being saved
    let activating = ""; // install root being set active
    let scanning = ""; // install root being scanned
    let changing = false; // Change install (folder picker → SetInstall)
    let syncing = false;
    let syncResult: SyncResult | null = null;
    let drafts: Record<string, string> = {};
    let refocus: { sel: string; pos: number } | null = null;

    const load = async (): Promise<void> => {
      loading = true;
      rerender();
      try {
        const [c, i, state] = await Promise.all([
          service.Config(),
          service.InstallsStatus(),
          service.GetState(),
        ]);
        cfg = c;
        installs = i;
        version = state.version;
        drafts = {
          wow_path: c.wow_path,
          flavor: c.flavor,
          profile: c.profile,
          collection: c.collection,
          backups_dir: c.backups_dir,
          curseforge_api_key: c.curseforge_api_key,
          collections_dir: c.collections_dir,
        };
        try {
          profiles = await service.Profiles();
        } catch {
          profiles = [];
        }
      } catch (err) {
        toast({
          type: "error",
          title: "Could not load settings",
          message: errText(err),
        });
      } finally {
        loading = false;
        rerender();
      }
    };

    // --- config save -----------------------------------------------------

    const saveKey = async (key: string, value: string): Promise<void> => {
      if (saving) return;
      saving = key;
      rerender();
      try {
        await service.SetConfigKey(key, value);
        if (cfg) {
          const coerced = coerceValue(key, value);
          (cfg as unknown as Record<string, unknown>)[key] = coerced;
        }
        drafts[key] = value;
        toast({
          type: "ok",
          title: `${KEY_LABELS[key] ?? key} saved`,
          message: typeof coerceValue(key, value) === "boolean"
            ? coerceValue(key, value) ? "on" : "off"
            : value,
        });
      } catch (err) {
        toast({
          type: "error",
          title: `Could not save ${KEY_LABELS[key] ?? key}`,
          message: errText(err),
        });
      } finally {
        saving = null;
        rerender();
      }
    };

    // --- installs --------------------------------------------------------

    const refreshInstalls = async (): Promise<void> => {
      try {
        installs = await service.InstallsStatus();
        installsError = null;
      } catch (err) {
        installsError = errText(err);
      }
      rerender();
    };

    // Change install: native folder picker → SetInstall with an empty flavor
    // (the backend derives it) → SetProfile from the returned install. The
    // full load() refresh keeps config, installs and profiles in sync.
    const changeInstall = async (): Promise<void> => {
      if (changing) return;
      changing = true;
      rerender();
      try {
        const picked = await pickFolder();
        if (!picked) return;
        const installed: Install = await service.SetInstall(picked, "");
        if (installed.profile_id) await service.SetProfile(installed.profile_id);
        toast({
          type: "ok",
          title: "Active install changed",
          message: picked,
        });
        await load();
      } catch (err) {
        toast({
          type: "error",
          title: "Could not change install",
          message: errText(err),
        });
      } finally {
        changing = false;
        rerender();
      }
    };

    const setActive = async (inst: InstallStatus): Promise<void> => {
      if (activating || scanning || syncing) return;
      activating = inst.root;
      rerender();
      try {
        const installed: Install = await service.SetInstall(inst.root, inst.flavor);
        await service.SetProfile(installed.profile_id || inst.profile_id);
        toast({
          type: "ok",
          title: "Active install switched",
          message: `${inst.flavor || "install"} — ${inst.root}`,
        });
      } catch (err) {
        toast({
          type: "error",
          title: "Could not switch install",
          message: errText(err),
        });
      } finally {
        activating = "";
        await refreshInstalls();
      }
    };

    const scanInstall = async (inst: InstallStatus): Promise<void> => {
      if (activating || scanning || syncing) return;
      scanning = inst.root;
      rerender();
      try {
        const installed: Install = await service.SetInstall(inst.root, inst.flavor);
        await service.SetProfile(installed.profile_id || inst.profile_id);
        const res = await service.Scan();
        const s = res.stats;
        toast({
          type: s.errors > 0 ? "warn" : "ok",
          title: `Scanned ${inst.flavor || "install"}`,
          message: `${s.total} addons · ${s.problems} with issues · ${s.errors} errors`,
        });
      } catch (err) {
        toast({
          type: "error",
          title: "Scan failed",
          message: errText(err),
        });
      } finally {
        scanning = "";
        await refreshInstalls();
      }
    };

    const updateAll = async (): Promise<void> => {
      if (syncing || !installs) return;
      const ready = installs.installs.filter((i) => i.exists).length;
      if (ready === 0) return;
      const confirmed = await confirmDialog({
        title: `Update all installs (${ready})?`,
        message:
          "Every detected install is checked against its providers and updated to the latest matching version. Flavor-mismatched updates are skipped.",
        confirmLabel: "Update All",
      });
      if (!confirmed) return;
      syncing = true;
      syncResult = null;
      rerender();
      try {
        const res = await service.SyncUpdatesToAll(true);
        syncResult = res;
        const touched = res.installs.filter(
          (i) => i.updated > 0 || i.failed > 0,
        ).length;
        toast({
          type:
            res.total_failed > 0
              ? res.total_updated > 0
                ? "warn"
                : "error"
              : "ok",
          title:
            res.total_failed > 0
              ? "Update all completed with errors"
              : "Update all complete",
          message: `${res.total_updated} updated · ${res.total_failed} failed across ${touched} install${touched === 1 ? "" : "s"}`,
        });
      } catch (err) {
        toast({
          type: "error",
          title: "Update all failed",
          message: errText(err),
        });
      } finally {
        syncing = false;
        await refreshInstalls();
      }
    };

    // --- render ----------------------------------------------------------

    const rerender = (): void => {
      render();
      if (!refocus) return;
      const target = host.querySelector<HTMLInputElement>(refocus.sel);
      if (target) {
        target.focus();
        if (refocus.pos >= 0) {
          const pos = Math.min(refocus.pos, target.value.length);
          target.setSelectionRange(pos, pos);
        }
      }
      refocus = null;
    };

    const render = (): void => {
      // Snapshot the Advanced disclosure before innerHTML is rebuilt: inline
      // edits re-render on every keystroke, and a fresh <details> is closed by
      // default — it would otherwise slam shut mid-typing.
      const advancedOpen =
        (host.querySelector(".settings-advanced") as HTMLDetailsElement | null)?.open ?? false;
      if (loading) {
        host.innerHTML = `
          <section class="view-page settings-page">
            <div class="view-hero">
              <h1 class="view-title">Settings</h1>
              <p class="view-sub">Configuration, installations and app information.</p>
            </div>
            <div class="list-loading"><span class="loading-pulse"></span><span>Loading settings…</span></div>
          </section>`;
        return;
      }

      host.innerHTML = `
        <section class="view-page settings-page">
          <div class="view-hero">
            <h1 class="view-title">Settings</h1>
            <p class="view-sub">Configuration, installations and app information.</p>
          </div>

          ${renderActiveInstallSection()}
          ${renderTogglesSection()}
          ${renderAboutSection()}
          ${renderAdvancedSection(advancedOpen)}
        </section>`;

      host.querySelectorAll<HTMLInputElement>("[data-text]").forEach((input) => {
        const key = input.dataset.text ?? "";
        input.addEventListener("input", () => {
          drafts[key] = input.value;
          refocus = {
            sel: `[data-text="${key}"]`,
            pos: input.selectionStart ?? input.value.length,
          };
          rerender();
        });
        input.addEventListener("keydown", (e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            void saveKey(key, input.value.trim());
          }
        });
      });
      host.querySelectorAll<HTMLElement>("[data-save]").forEach((btn) => {
        btn.addEventListener("click", () => {
          const key = btn.dataset.save ?? "";
          void saveKey(key, (drafts[key] ?? "").trim());
        });
      });
      host.querySelectorAll<HTMLInputElement>("[data-bool]").forEach((input) => {
        input.addEventListener("change", () => {
          void saveKey(input.dataset.bool ?? "", input.checked ? "true" : "false");
        });
      });
      host.querySelectorAll<HTMLElement>("[data-active]").forEach((btn) => {
        const inst = installs?.installs[Number(btn.dataset.active)];
        if (inst) btn.addEventListener("click", () => void setActive(inst));
      });
      host.querySelectorAll<HTMLElement>("[data-scan]").forEach((btn) => {
        const inst = installs?.installs[Number(btn.dataset.scan)];
        if (inst) btn.addEventListener("click", () => void scanInstall(inst));
      });
      host.querySelector("[data-update-all]")?.addEventListener("click", () => {
        void updateAll();
      });
      host.querySelector("[data-change-install]")?.addEventListener("click", () => {
        void changeInstall();
      });
    };

    const renderActiveInstallSection = (): string => {
      // The active install is whatever the saved config points at; fall back
      // to the first existing detected install so a zero-config adopt still
      // has a card. installs.active is not part of the InstallsStatusResult
      // contract, so the root comes from cfg.wow_path (or the match).
      const activeRoot =
        cfg?.wow_path || installs?.installs.find((i) => i.exists)?.root || "";
      const activeInst = installs?.installs.find((i) => i.root === activeRoot);
      const flavor = activeInst?.flavor || cfg?.flavor || "";
      const profileId = activeInst?.profile_id || cfg?.profile || "";
      const profileName =
        profiles.find((p) => p.id === profileId)?.name || profileId || "";
      return `
        <section class="settings-section" aria-labelledby="settings-active-title">
          <div class="section-head">
            <div class="section-head-text">
              <h2 id="settings-active-title" class="section-title">Active install</h2>
              <p class="section-desc">The install wowfix scans, repairs and updates. Switch anytime — the active profile follows.</p>
            </div>
          </div>
          <div class="card active-install-card">
            <div class="active-install-grid">
              <div class="active-install-item">
                <span class="active-install-key">Install</span>
                ${
                  activeRoot
                    ? `<span class="active-install-val mono" title="${escapeAttr(activeRoot)}">${escapeHtml(truncateMiddle(activeRoot, 52))}</span>`
                    : `<span class="active-install-val active-install-empty">No install selected</span>`
                }
              </div>
              <div class="active-install-item">
                <span class="active-install-key">Flavor</span>
                <span class="active-install-val">${escapeHtml(flavorLabel(flavor))}</span>
              </div>
              <div class="active-install-item">
                <span class="active-install-key">Profile</span>
                <span class="active-install-val">${profileName ? escapeHtml(profileName) : '<span class="active-install-empty">—</span>'}</span>
              </div>
            </div>
            <div class="active-install-actions">
              <button class="btn-secondary" data-change-install ${changing ? "disabled" : ""}>
                ${changing ? icon("refresh", 15) : icon("folder", 15)}
                <span>${changing ? "Changing…" : "Change install…"}</span>
              </button>
            </div>
          </div>
        </section>`;
    };

    const renderTogglesSection = (): string => `
      <section class="settings-section" aria-labelledby="settings-behavior-title">
        <div class="section-head">
          <div class="section-head-text">
            <h2 id="settings-behavior-title" class="section-title">Behavior</h2>
            <p class="section-desc">How wowfix acts during repairs and updates.</p>
          </div>
        </div>
        ${
          cfg
            ? `<div class="card cfg-card">${BOOL_KEYS.map(renderConfigRow).join("")}</div>`
            : `<div class="card"><p class="section-desc">Behavior settings unavailable.</p></div>`
        }
      </section>`;

    const renderAdvancedSection = (open: boolean): string => `
      <details class="settings-advanced" ${open ? "open" : ""}>
        <summary>
          ${icon("chevron-down", 15)}
          <span class="advanced-summary-title">Advanced</span>
          <span class="advanced-summary-hint">raw config keys · all installs</span>
        </summary>
        ${renderConfigSection()}
        ${renderInstallsSection()}
      </details>`;

    const renderConfigSection = (): string => {
      const groups = cfg
        ? GROUP_ORDER.map(
            (g) => `
          <div class="cfg-group">${escapeHtml(g.title)}</div>
          ${g.keys.map((key) => renderConfigRow(key)).join("")}`,
          ).join("")
        : "";

      return `
        <section class="settings-section" aria-labelledby="settings-config-title">
          <div class="section-head">
            <div class="section-head-text">
              <h2 id="settings-config-title" class="section-title">Configuration</h2>
              <p class="section-desc">Editable key/value pairs. Install-derived keys are normally managed from Setup and the Installations section.</p>
            </div>
          </div>
          ${cfgError ? `<p class="section-error" role="alert">${icon("x-circle", 15)}<span>${escapeHtml(cfgError)}</span></p>` : ""}
          ${
            cfg
              ? `<div class="card cfg-card">${groups}</div>`
              : `<div class="card"><p class="section-desc">Configuration unavailable.</p></div>`
          }
        </section>`;
    };

    const renderConfigRow = (key: string): string => {
      const c = cfg as unknown as Record<string, unknown>;
      if (BOOL_KEYS.includes(key as (typeof BOOL_KEYS)[number])) {
        const on = Boolean(c[key]);
        return `
          <div class="cfg-row">
            <div class="cfg-key">
              <span class="cfg-label">${escapeHtml(KEY_LABELS[key] ?? key)}</span>
              <span class="cfg-hint">${escapeHtml(KEY_HINTS[key] ?? "")}</span>
            </div>
            <label class="switch cfg-value">
              <input type="checkbox" data-bool="${key}" ${on ? "checked" : ""} ${saving ? "disabled" : ""} aria-label="${escapeAttr(KEY_LABELS[key] ?? key)}" />
              <span class="switch-track"></span>
              <span class="switch-thumb"></span>
            </label>
          </div>`;
      }
      const value = drafts[key] ?? String(c[key] ?? "");
      const isSecret = key === "curseforge_api_key";
      const isSaving = saving === key;
      const dirty = value !== String(c[key] ?? "");
      return `
        <div class="cfg-row">
          <div class="cfg-key">
            <span class="cfg-label">${escapeHtml(KEY_LABELS[key] ?? key)}</span>
            <span class="cfg-hint">${escapeHtml(KEY_HINTS[key] ?? "")}</span>
          </div>
          <div class="cfg-field">
            <input class="text-input cfg-input${key === "wow_path" || key === "backups_dir" || key === "collections_dir" ? " mono" : ""}" id="settings-${key}"
              type="${isSecret ? "password" : "text"}" spellcheck="false" autocomplete="off"
              value="${escapeAttr(value)}" data-text="${key}" ${saving ? "disabled" : ""} />
            <button class="btn-secondary" data-save="${key}" ${saving || !dirty ? "disabled" : ""}>
              ${isSaving ? icon("refresh", 15) : icon("check", 15)}
              <span>${isSaving ? "Saving…" : "Save"}</span>
            </button>
          </div>
        </div>`;
    };

    const renderInstallsSection = (): string => {
      const list = installs?.installs ?? [];
      const ready = list.filter((i) => i.exists).length;
      const busy = Boolean(activating || scanning || syncing);
      return `
        <section class="settings-section" aria-labelledby="settings-installs-title">
          <div class="section-head">
            <div class="section-head-text">
              <h2 id="settings-installs-title" class="section-title">Installations</h2>
              <p class="section-desc">Every WoW install detected on this machine. Set the active one, scan its addons, or update all of them in one pass.</p>
            </div>
            <div class="section-toolbar">
              <button class="btn-primary" data-update-all ${busy || ready === 0 ? "disabled" : ""}>
                ${syncing ? icon("refresh", 15) : icon("download", 15)}
                <span>${syncing ? "Updating…" : `Update all installs (${ready})`}</span>
              </button>
              ${installs ? `<span class="toolbar-summary">${list.length} install${list.length === 1 ? "" : "s"} detected · ${ready} ready</span>` : ""}
            </div>
          </div>

          ${installsError ? `<p class="section-error" role="alert">${icon("x-circle", 15)}<span>${escapeHtml(installsError)}</span></p>` : ""}
          ${syncResult ? renderSyncResult(syncResult) : ""}

          ${
            !installs
              ? `<div class="list-loading"><span class="loading-pulse"></span><span>Checking installs…</span></div>`
              : list.length === 0
                ? `<div class="card"><p class="section-desc">No WoW installations detected. Point wowfix at one from Setup.</p></div>`
                : `<div class="install-cards">${list.map((inst, i) => renderInstallCard(inst, i, busy)).join("")}</div>`
          }
        </section>`;
    };

    const renderInstallCard = (inst: InstallStatus, i: number, busy: boolean): string => {
      const band: "ok" | "warn" | "error" =
        inst.health >= 85 ? "ok" : inst.health >= 60 ? "warn" : "error";
      const bandLabel =
        band === "ok" ? "Healthy" : band === "warn" ? "Needs attention" : "Needs repair";
      const isBusy = busy || activating === inst.root || scanning === inst.root;

      const head = `
        <div class="install-card-head">
          <span class="install-path mono" title="${escapeAttr(inst.root)}">${escapeHtml(truncateMiddle(inst.root, 46))}</span>
          ${inst.flavor ? `<span class="flavor-tag">${escapeHtml(inst.flavor)}</span>` : ""}
          ${inst.version ? `<span class="install-version mono">v${escapeHtml(inst.version)}</span>` : ""}
          ${inst.confidence ? `<span class="confidence-chip ${confClass(inst.confidence)}">${escapeHtml(inst.confidence)} confidence</span>` : ""}
        </div>`;

      if (!inst.exists) {
        return `
          <div class="install-card missing">
            ${head}
            <div class="install-missing">${icon("alert", 16)}<span>AddOns folder not found</span></div>
          </div>`;
      }

      return `
        <div class="install-card">
          ${head}
          <div class="install-band">
            <span class="install-score">${inst.health}</span>
            <span class="install-denom">/100</span>
            <span class="install-band-label ${band}">${bandLabel}</span>
            <span class="install-healthbar ${band}"><i style="width:${Math.max(0, Math.min(100, inst.health))}%"></i></span>
          </div>
          <div class="install-stats">
            <span class="stat-item"><span class="status-dot ok"></span><span class="stat-num">${inst.addons}</span> addons</span>
            <span class="stat-item"><span class="status-dot warn"></span><span class="stat-num">${inst.problems}</span> with issues</span>
            <span class="stat-item"><span class="status-dot ${inst.errors > 0 ? "error" : ""}"></span><span class="stat-num">${inst.errors}</span> error${inst.errors === 1 ? "" : "s"}</span>
          </div>
          <div class="install-actions">
            <button class="btn-secondary" data-scan="${i}" ${busy ? "disabled" : ""}>
              ${icon("refresh", 15)}
              <span>${scanning === inst.root ? "Scanning…" : "Scan"}</span>
            </button>
            <button class="btn-primary" data-active="${i}" ${isBusy ? "disabled" : ""}>
              ${activating === inst.root ? icon("refresh", 15) : icon("check", 15)}
              <span>${activating === inst.root ? "Setting…" : "Set as active"}</span>
            </button>
          </div>
        </div>`;
    };

    const renderSyncResult = (res: SyncResult): string => `
      <div class="sync-result" role="status">
        <div class="sync-summary">
          <span class="sync-summary-icon ${res.total_failed > 0 ? "has-errors" : ""}">
            ${icon(res.total_failed > 0 ? "alert" : "check-circle", 18)}
          </span>
          <span><b>${res.total_updated} updated</b>
            ${res.total_failed > 0 ? ` · ${res.total_failed} failed` : ""}
            <span class="muted"> across ${res.installs.length} install${res.installs.length === 1 ? "" : "s"}</span>
          </span>
        </div>
        ${res.installs.some((r) => r.failed > 0 || r.errors.length > 0)
          ? `<div class="sync-rows">
              ${res.installs
                .map((r) => {
                  if (r.failed === 0 && r.errors.length === 0) return "";
                  return `
                <div class="sync-row">
                  <span class="sync-root mono">${escapeHtml(truncateMiddle(r.root, 60))}</span>
                  <span class="section-desc">${r.updated} updated · ${r.failed} failed</span>
                  ${
                    r.errors.length
                      ? `<ul class="sync-errors">${r.errors
                          .map((e) => `<li>${icon("x-circle", 13)}<span>${escapeHtml(e)}</span></li>`)
                          .join("")}</ul>`
                      : ""
                  }
                </div>`;
                })
                .join("")}
            </div>`
          : ""}
      </div>`;

    const renderAboutSection = (): string => `
      <section class="settings-section" aria-labelledby="settings-about-title">
        <div class="section-head">
          <div class="section-head-text">
            <h2 id="settings-about-title" class="section-title">About</h2>
          </div>
        </div>
        <div class="card about-card">
          <div class="about-version">
            <span class="about-app">wowfix</span>
            <span class="about-ver">v${escapeHtml(version || "—")}</span>
            ${cfg?.flavor ? `<span class="flavor-tag">${escapeHtml(cfg.flavor)}</span>` : ""}
          </div>
          <div class="about-meta">
            ${cfg?.profile ? `<span>Profile: <span class="mono">${escapeHtml(cfg.profile)}</span></span>` : ""}
            ${cfg?.wow_path ? `<span>Addons: <span class="mono">${escapeHtml(cfg.wow_path)}\\Interface\\AddOns</span></span>` : ""}
          </div>
          <p class="about-note">Repair, update and back up your World of Warcraft addons — with every change snapshotted before it happens.</p>
        </div>
      </section>`;

    rerender();
    void load();
  },
};

/** Native folder picker when the Wails runtime is present; folder-input
 *  fallback otherwise (returns the selected folder's name — the browser
 *  never exposes absolute paths). Mirrors setup.ts. */
function pickFolder(): Promise<string | null> {
  const rt = (window as unknown as {
    runtime?: { OpenDirectoryDialog?: (opts: unknown) => Promise<string | null> };
  }).runtime;
  if (rt?.OpenDirectoryDialog) {
    return rt.OpenDirectoryDialog({
      title: "Select your World of Warcraft install",
    });
  }
  return new Promise<string | null>((resolve) => {
    const input = document.createElement("input");
    input.type = "file";
    input.setAttribute("webkitdirectory", "");
    input.setAttribute("directory", "");
    input.style.display = "none";
    input.addEventListener("change", () => {
      const first = input.files?.[0];
      resolve(first ? (first.webkitRelativePath.split("/")[0] || null) : null);
      input.remove();
    });
    document.body.appendChild(input);
    input.click();
  });
}

/** Display labels for install flavors; accepts both "_retail_" (backend
 *  install flavor) and "retail" (config flavor) spellings. Unknown flavors
 *  fall back to the raw value. */
const FLAVOR_LABEL: Record<string, string> = {
  retail: "Retail",
  classic: "Wrath Classic",
  classic_era: "Classic Era",
  classic_tbc: "TBC Classic",
  root: "Top-level",
};

function flavorLabel(flavor: string | undefined | null): string {
  if (!flavor) return "Top-level";
  const key = flavor.replace(/^_+|_+$/g, "");
  if (key === "root") return "Top-level";
  return FLAVOR_LABEL[key] ?? flavor;
}

function coerceValue(key: string, value: string): unknown {
  if (key === "auto_backup" || key === "confirmations") return value === "true";
  return value;
}

function confClass(confidence: string): "ok" | "warn" | "error" {
  switch (confidence) {
    case "high":
      return "ok";
    case "medium":
      return "warn";
    default:
      return "error";
  }
}

function truncateMiddle(s: string, max: number): string {
  if (s.length <= max) return s;
  const head = Math.ceil((max - 1) / 2);
  const tail = Math.floor((max - 1) / 2);
  return `${s.slice(0, head)}…${s.slice(-tail)}`;
}

function errText(err: unknown, fallback?: string): string {
  if (err instanceof Error && err.message) return err.message;
  if (fallback) return fallback;
  return String(err ?? "Unknown error");
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ESC[c]);
}
function escapeAttr(s: string): string {
  return escapeHtml(s).replaceAll("'", "&#39;");
}
const ESC: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
};
