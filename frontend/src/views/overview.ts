// Overview — the default landing view. Hosts the three health workflows
// (Scan / Doctor / Validation) behind a pill segmented control. Scan is
// fetched on mount; Doctor and Validation are lazy per first activation.
// LEARNINGS adoptions: problem rows sorted first, per-addon status chips
// keyed to the right addon, row-level failure surfacing (never a blanket
// success toast), badge-counted Fix All, end-of-batch error dump.

import type { View } from "../view";
import type {
  Addon,
  AddonStatus,
  CheckStatus,
  CompatStatus,
  DoctorCheck,
  DoctorReport,
  FixResult,
  ScanResult,
  ValidateResult,
} from "../types";
import { service } from "../api";
import { icon, type IconName } from "../icons";
import { toast } from "../toast";
import "./overview.css";

type Segment = "scan" | "doctor" | "validate";

const SEGMENTS: { value: Segment; label: string; icon: IconName }[] = [
  { value: "scan", label: "Scan", icon: "search" },
  { value: "doctor", label: "Doctor", icon: "shield" },
  { value: "validate", label: "Validation", icon: "check-circle" },
];

// Problem rows surface first (error → warn → ok).
const SEVERITY_ORDER: Record<AddonStatus, number> = { error: 0, warn: 1, ok: 2 };

// Only one view mounts at a time (the shell unmounts before remounting),
// so a single flag is enough to stop post-await renders from clobbering
// the host after the user has switched away mid-flight.
let disposed: { gone: boolean } | null = null;

export const view: View = {
  id: "overview",
  label: "Overview",
  icon: "shield",
  mount(host) {
    disposed = { gone: false };
    const isGone = () => disposed?.gone ?? false;
    let segment: Segment = "scan";
    let busyScan: null | "scan" | "fix" | "fixall" = null;
    let fixingFolder: string | null = null;
    let busyDoctor = false;
    let busyValidate = false;

    const state: {
      scan: ScanResult | null;
      doctor: DoctorReport | null;
      validate: ValidateResult | null;
      fixError: Record<string, string>;
      fixDump: FixResult | null;
    } = {
      scan: null,
      doctor: null,
      validate: null,
      fixError: {},
      fixDump: null,
    };

    const render = (): void => {
      host.innerHTML = `
        <section class="view-page ov-page">
          <header class="view-hero">
            <h1 class="view-title">Overview</h1>
            <p class="view-sub">Your install at a glance — scan for repair problems, run
              diagnostics and validate TOC compatibility.</p>
          </header>
          <div class="tabs ov-tabs" role="tablist" aria-label="Health workflows">
            ${SEGMENTS.map(
              (s) => `
              <button class="tab" role="tab" id="ov-tab-${s.value}" data-seg="${s.value}"
                aria-selected="${segment === s.value}" aria-controls="ov-panel-${s.value}"
                tabindex="${segment === s.value ? 0 : -1}">
                ${icon(s.icon, 15)}${s.label}
              </button>`,
            ).join("")}
          </div>
          <div class="ov-panel" id="ov-panel-${segment}" role="tabpanel"
            aria-labelledby="ov-tab-${segment}"></div>
        </section>`;

      const tabButtons = Array.from(
        host.querySelectorAll<HTMLButtonElement>('[role="tab"]'),
      );
      tabButtons.forEach((btn) => {
        btn.addEventListener("click", () => {
          activate(btn.dataset.seg as Segment);
        });
        btn.addEventListener("keydown", (e) => {
          if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
          e.preventDefault();
          const idx = SEGMENTS.findIndex((s) => s.value === segment);
          const next =
            (idx + (e.key === "ArrowRight" ? 1 : SEGMENTS.length - 1)) %
            SEGMENTS.length;
          activate(SEGMENTS[next].value);
          host
            .querySelector<HTMLButtonElement>(`#ov-tab-${SEGMENTS[next].value}`)
            ?.focus();
        });
      });

      const panel = host.querySelector<HTMLElement>(".ov-panel")!;
      switch (segment) {
        case "scan":
          renderScan(panel);
          break;
        case "doctor":
          renderDoctor(panel);
          break;
        case "validate":
          renderValidate(panel);
          break;
      }
    };

    const activate = (seg: Segment): void => {
      if (seg === segment) return;
      segment = seg;
      render();
      if (seg === "doctor" && !state.doctor && !busyDoctor) void runDoctor();
      if (seg === "validate" && !state.validate && !busyValidate) {
        void runValidate();
      }
    };

    // ---------- scan ----------

    async function refreshScan(): Promise<void> {
      busyScan = "scan";
      render();
      try {
        state.scan = await service.Scan();
      } catch (err) {
        toast({ type: "error", title: "Scan failed", message: errText(err) });
      } finally {
        busyScan = null;
        if (!isGone()) render();
      }
    }

    async function fixOne(folder: string): Promise<void> {
      if (busyScan) return;
      busyScan = "fix";
      fixingFolder = folder;
      render();
      try {
        const res = await service.Fix(folder, true);
        const entry = res.fixes[0];
        if (entry && entry.ok) {
          delete state.fixError[folder];
          toast({ type: "ok", title: `${folder} repaired`, message: entry.message });
        } else {
          const msg = entry?.message || entry?.error || "Fix failed";
          state.fixError[folder] = msg;
          toast({
            type: "error",
            title: `Could not repair ${folder}`,
            message: msg,
          });
        }
      } catch (err) {
        const msg = errText(err, "Fix failed");
        state.fixError[folder] = msg;
        toast({ type: "error", title: `Could not repair ${folder}`, message: msg });
      } finally {
        busyScan = null;
        fixingFolder = null;
        await refreshScan();
      }
    }

    async function fixAll(): Promise<void> {
      const scan = state.scan;
      if (!scan || busyScan) return;
      const fixable = scan.addons.filter((a) => a.fixable);
      if (fixable.length === 0) return;
      busyScan = "fixall";
      render();
      try {
        const res = await service.FixAll(true);
        if (res.failed > 0) {
          state.fixDump = res;
          toast({
            type: "error",
            title: `Repair finished with ${res.failed} failure${res.failed === 1 ? "" : "s"}`,
            message: `${res.fixed} repaired · ${res.failed} failed`,
          });
        } else {
          toast({
            type: "ok",
            title: `${res.fixed} addon${res.fixed === 1 ? "" : "s"} repaired`,
          });
        }
      } catch (err) {
        toast({ type: "error", title: "Fix All failed", message: errText(err) });
      } finally {
        busyScan = null;
        await refreshScan();
      }
    }

    const renderScan = (el: HTMLElement): void => {
      const scan = state.scan;
      if (!scan) {
        el.innerHTML = `<div class="ov-loading"><span class="ov-spinner"></span><span>Scanning addons…</span></div>`;
        return;
      }
      // Drop stale row errors (renamed or since-repaired folders).
      for (const k of Object.keys(state.fixError)) {
        const a = scan.addons.find((x) => x.folder_name === k);
        if (!a || a.status === "ok") delete state.fixError[k];
      }

      const { addons, stats } = scan;
      const sorted = [...addons].sort(bySeverity);
      const fixable = addons.filter((a) => a.fixable).length;
      const busy = busyScan !== null;

      const errorsHtml =
        scan.errors.length > 0
          ? `<div class="ov-errors" role="alert">
              <div class="ov-errors-head">${icon("alert", 15)}<span>${scan.errors.length} problem${scan.errors.length === 1 ? "" : "s"} while scanning</span></div>
              <ul>${scan.errors.map((e) => `<li>${escapeHtml(e)}</li>`).join("")}</ul>
            </div>`
          : "";

      const dumpHtml = state.fixDump
        ? `<div class="ov-dump" role="alert">
            <div class="ov-dump-head">
              <span class="ov-dump-title">${icon("x-circle", 15)} ${state.fixDump.failed} repair${state.fixDump.failed === 1 ? "" : "s"} failed</span>
              <button class="icon-btn ov-dump-close" data-dump-close aria-label="Dismiss">${icon("x", 14)}</button>
            </div>
            <ul>
              ${state.fixDump.fixes
                .filter((f) => !f.ok)
                .map(
                  (f) =>
                    `<li><span class="mono">${escapeHtml(f.addon)}</span> — ${escapeHtml(f.message || f.error || "")}</li>`,
                )
                .join("")}
            </ul>
          </div>`
        : "";

      const headHtml = `
        <div class="ov-panel-head">
          <div class="ov-counts">
            <span class="ov-stat"><span class="ov-dot -ok"></span>${stats.total} addons</span>
            <span class="ov-stat"><span class="ov-dot -warn"></span>${stats.problems} with issues</span>
            ${stats.errors > 0 ? `<span class="ov-stat"><span class="ov-dot -error"></span>${stats.errors} error${stats.errors === 1 ? "" : "s"}</span>` : ""}
          </div>
          <div class="ov-actions">
            <button class="btn-secondary" data-rescan ${busy ? "disabled" : ""}>
              ${busyScan === "scan" ? `<span class="ov-spinner -dark"></span>` : icon("refresh", 15)}
              <span>${busyScan === "scan" ? "Scanning…" : "Rescan"}</span>
            </button>
            <button class="btn-primary" data-fixall ${busy || fixable === 0 ? "disabled" : ""}>
              ${icon("check", 15)}<span>Fix All</span>
              ${fixable > 0 ? `<span class="ov-badge">${fixable}</span>` : ""}
            </button>
            <span class="ov-fixall-note">Backed up first · removals go to the OS trash</span>
          </div>
        </div>`;

      if (sorted.length === 0) {
        el.innerHTML = `${errorsHtml}${headHtml}
          <div class="empty-state">
            <span class="empty-title">No addons found</span>
            <span class="empty-body">Interface/AddOns is empty or unreadable. Add addons, then rescan.</span>
          </div>`;
      } else {
        el.innerHTML = `${errorsHtml}${headHtml}${dumpHtml}
          <div class="ov-table-wrap">
            <table class="ov-table">
              <thead><tr>
                <th class="ov-col-status">Status</th>
                <th>Addon</th>
                <th class="ov-col-ver">Version</th>
                <th class="ov-col-src">Source</th>
                <th class="ov-col-issues">Issues</th>
                <th class="ov-col-actions">Fix</th>
              </tr></thead>
              <tbody>${sorted.map(rowHtml).join("")}</tbody>
            </table>
          </div>`;
      }

      el.querySelector("[data-rescan]")?.addEventListener("click", () => {
        void refreshScan();
      });
      el.querySelector("[data-fixall]")?.addEventListener("click", () => {
        void fixAll();
      });
      el.querySelector("[data-dump-close]")?.addEventListener("click", () => {
        state.fixDump = null;
        render();
      });
      el.querySelectorAll<HTMLElement>("[data-fix]").forEach((btn) => {
        btn.addEventListener("click", () => {
          void fixOne(String(btn.dataset.fix));
        });
      });
    };

    const rowHtml = (a: Addon): string => {
      const firstIssue = a.issues[0];
      const more =
        a.issues.length > 1 ? ` <span class="ov-more">+${a.issues.length - 1} more</span>` : "";
      const rename =
        a.suggested_name && a.suggested_name !== a.folder_name
          ? `<span class="ov-rename mono">→ ${escapeHtml(a.suggested_name)}</span>`
          : "";
      const tags = [
        a.nested ? `<span class="ov-tag">nested</span>` : "",
        a.tracked ? `<span class="ov-tag -tracked">tracked</span>` : "",
        a.drifted
          ? `<span class="ov-tag -drifted">${icon("alert", 11)}modified</span>`
          : "",
      ].join("");
      const version = a.toc?.version ? `v${escapeHtml(a.toc.version)}` : "n/a";
      const source = a.tracked_source ? escapeHtml(a.tracked_source) : "—";
      const err = state.fixError[a.folder_name];
      const fixLabel = a.issues.find((i) => i.action)?.action_label ?? "Fix";
      const fixing = busyScan === "fix" && fixingFolder === a.folder_name;
      const fixBtn = a.fixable
        ? `<button class="btn-primary ov-fix-btn" data-fix="${escapeAttr(a.folder_name)}" ${busyScan !== null ? "disabled" : ""}>
            ${fixing ? `<span class="ov-spinner -dark"></span>` : ""}<span>${escapeHtml(fixLabel)}</span>
          </button>`
        : "";
      return `
        <tr class="ov-row -${a.status}">
          <td><span class="chip ov-chip -${a.status}">${icon(statusIcon(a.status), 12)}${a.status}</span></td>
          <td>
            <div class="ov-addon">
              <span class="ov-addon-name">${escapeHtml(a.folder_name)}</span>
              ${tags}
            </div>
            ${rename}
          </td>
          <td class="ov-ver mono tnum">${version}</td>
          <td class="ov-src mono" title="${escapeAttr(a.tracked_source ?? "")}">${source}</td>
          <td class="ov-issues">
            ${err ? `<span class="ov-row-error">${icon("x-circle", 12)}${escapeHtml(err)}</span>` : ""}
            ${!err && firstIssue ? `<span class="ov-issue-msg">${escapeHtml(firstIssue.message)}</span>${more}` : ""}
          </td>
          <td class="ov-actions">${fixBtn}</td>
        </tr>`;
    };

    // ---------- doctor ----------

    async function runDoctor(): Promise<void> {
      if (busyDoctor) return;
      busyDoctor = true;
      render();
      try {
        state.doctor = await service.Doctor();
        const errors = state.doctor.checks.filter((c) => c.status === "error").length;
        const warns = state.doctor.checks.filter((c) => c.status === "warn").length;
        toast({
          type: errors > 0 ? "error" : warns > 0 ? "warn" : "ok",
          title: "Diagnostics complete",
          message: `${state.doctor.checks.length} check${state.doctor.checks.length === 1 ? "" : "s"} · ${errors} error${errors === 1 ? "" : "s"} · ${warns} warning${warns === 1 ? "" : "s"}`,
        });
      } catch (err) {
        toast({ type: "error", title: "Diagnostics failed", message: errText(err) });
      } finally {
        busyDoctor = false;
        if (!isGone()) render();
      }
    }

    const renderDoctor = (el: HTMLElement): void => {
      const report = state.doctor;
      if (!report && !busyDoctor) {
        el.innerHTML = `
          <div class="ov-panel-head">
            <div class="ov-counts ov-counts-sub">Install · addons · updates · saved variables · backups</div>
            <div class="ov-actions">
              <button class="btn-secondary" data-doctor>${icon("shield", 15)}<span>Run diagnostics</span></button>
            </div>
          </div>
          <div class="empty-state">
            <span class="empty-title">Doctor</span>
            <span class="empty-body">Run a diagnostic pass over the install, the addon folder, update state, saved variables and backup health.</span>
          </div>`;
        el.querySelector("[data-doctor]")?.addEventListener("click", () => {
          void runDoctor();
        });
        return;
      }
      if (!report) {
        el.innerHTML = `<div class="ov-loading"><span class="ov-spinner"></span><span>Running diagnostics…</span></div>`;
        return;
      }
      const errors = report.checks.filter((c) => c.status === "error").length;
      const warns = report.checks.filter((c) => c.status === "warn").length;
      const machine: CheckStatus = errors > 0 ? "error" : warns > 0 ? "warn" : "ok";
      const machineLabel =
        errors > 0 ? "Needs attention" : warns > 0 ? "Degraded" : "Healthy";
      el.innerHTML = `
        <div class="ov-panel-head">
          <div class="ov-counts">
            <span class="chip ov-chip -${machine}">${icon(checkIcon(machine), 12)}${machineLabel}</span>
            <span class="ov-stat">${report.checks.length} check${report.checks.length === 1 ? "" : "s"}</span>
            ${errors > 0 ? `<span class="ov-stat"><span class="ov-dot -error"></span>${errors} error${errors === 1 ? "" : "s"}</span>` : ""}
            ${warns > 0 ? `<span class="ov-stat"><span class="ov-dot -warn"></span>${warns} warning${warns === 1 ? "" : "s"}</span>` : ""}
          </div>
          <div class="ov-actions">
            <button class="btn-secondary" data-doctor ${busyDoctor ? "disabled" : ""}>
              ${busyDoctor ? `<span class="ov-spinner -dark"></span>` : icon("refresh", 15)}
              <span>${busyDoctor ? "Running…" : "Run diagnostics"}</span>
            </button>
          </div>
        </div>
        <div class="ov-check-list">
          ${report.checks.map(checkRow).join("")}
        </div>`;
      el.querySelector("[data-doctor]")?.addEventListener("click", () => {
        void runDoctor();
      });
    };

    const checkRow = (c: DoctorCheck): string => `
      <div class="ov-check">
        <span class="ov-check-name mono">${escapeHtml(c.name)}</span>
        <span class="chip ov-chip ${checkClass(c.status)}">${icon(checkIcon(c.status), 12)}${escapeHtml(c.status)}</span>
        <span class="ov-check-msg">${escapeHtml(c.message)}</span>
      </div>`;

    // ---------- validation ----------

    async function runValidate(): Promise<void> {
      if (busyValidate) return;
      busyValidate = true;
      render();
      try {
        state.validate = await service.Validate();
      } catch (err) {
        toast({ type: "error", title: "Validation failed", message: errText(err) });
      } finally {
        busyValidate = false;
        if (!isGone()) render();
      }
    }

    const renderValidate = (el: HTMLElement): void => {
      const v = state.validate;
      if (!v && !busyValidate) {
        el.innerHTML = `
          <div class="ov-panel-head">
            <div class="ov-counts ov-counts-sub">Every addon TOC checked against the profile's expected interface</div>
            <div class="ov-actions">
              <button class="btn-secondary" data-validate>${icon("check-circle", 15)}<span>Validate TOCs</span></button>
            </div>
          </div>
          <div class="empty-state">
            <span class="empty-title">Validation</span>
            <span class="empty-body">Checks every addon's .toc against the game versions the profile expects.</span>
          </div>`;
        el.querySelector("[data-validate]")?.addEventListener("click", () => {
          void runValidate();
        });
        return;
      }
      if (!v) {
        el.innerHTML = `<div class="ov-loading"><span class="ov-spinner"></span><span>Validating TOC files…</span></div>`;
        return;
      }
      const bad = v.addons.filter((a) => a.status !== "compatible").length;
      el.innerHTML = `
        <div class="ov-panel-head">
          <div class="ov-counts">
            <span class="ov-stat">Expected interface <b class="tnum">${v.expected}</b></span>
            <span class="ov-stat ${bad > 0 ? "ov-text-warn" : "ov-text-ok"}">${v.addons.length} addon${v.addons.length === 1 ? "" : "s"} · ${bad} not compatible</span>
          </div>
          <div class="ov-actions">
            <button class="btn-secondary" data-validate ${busyValidate ? "disabled" : ""}>
              ${busyValidate ? `<span class="ov-spinner -dark"></span>` : icon("refresh", 15)}
              <span>${busyValidate ? "Validating…" : "Re-validate"}</span>
            </button>
          </div>
        </div>
        ${
          v.addons.length === 0
            ? `<div class="empty-state">
                <span class="empty-title">Nothing to validate</span>
                <span class="empty-body">No addon folders with a TOC file were found.</span>
              </div>`
            : `<div class="ov-table-wrap">
                <table class="ov-table">
                  <thead><tr>
                    <th>Addon</th>
                    <th class="ov-col-toc">TOC</th>
                    <th class="ov-col-ver">Expected</th>
                    <th class="ov-col-ver">Detected</th>
                    <th class="ov-col-status">Verdict</th>
                  </tr></thead>
                  <tbody>
                    ${v.addons
                      .map(
                        (a) => `
                      <tr class="ov-row -${compatRowStatus(a.status)}">
                        <td><span class="ov-addon-name">${escapeHtml(a.folder_name)}</span></td>
                        <td class="mono">${escapeHtml(a.toc)}</td>
                        <td class="ov-ver mono tnum">${a.expected}</td>
                        <td class="ov-ver mono tnum">${a.detected > 0 ? a.detected : "n/a"}</td>
                        <td><span class="chip ov-chip ${compatClass(a.status)}">${icon(checkIcon(compatCheck(a.status)), 12)}${escapeHtml(a.label)}</span></td>
                      </tr>`,
                      )
                      .join("")}
                  </tbody>
                </table>
              </div>`
        }`;
      el.querySelector("[data-validate]")?.addEventListener("click", () => {
        void runValidate();
      });
    };

    // ---------- helpers ----------

    const bySeverity = (a: Addon, b: Addon): number =>
      SEVERITY_ORDER[a.status] - SEVERITY_ORDER[b.status] ||
      a.folder_name.localeCompare(b.folder_name);

    render();
    void refreshScan();
  },
  unmount() {
    if (disposed) disposed.gone = true;
  },
};

function statusIcon(status: AddonStatus): IconName {
  switch (status) {
    case "error":
      return "x-circle";
    case "warn":
      return "alert";
    default:
      return "check-circle";
  }
}

function checkIcon(status: CheckStatus): IconName {
  switch (status) {
    case "ok":
      return "check-circle";
    case "error":
      return "x-circle";
    case "warn":
      return "alert";
    default:
      return "info";
  }
}

function checkClass(status: CheckStatus): string {
  switch (status) {
    case "ok":
      return "-ok";
    case "warn":
      return "-warn";
    case "error":
      return "-error";
    default:
      return "-info";
  }
}

function compatClass(status: CompatStatus): string {
  switch (status) {
    case "compatible":
      return "-ok";
    case "mismatch":
      return "-error";
    default:
      return "-warn"; // vanilla, retail, unknown
  }
}

function compatCheck(status: CompatStatus): CheckStatus {
  switch (status) {
    case "compatible":
      return "ok";
    case "mismatch":
      return "error";
    default:
      return "warn";
  }
}

/** Row tint for the validate table (shared chip conventions). */
function compatRowStatus(status: CompatStatus): AddonStatus {
  switch (status) {
    case "compatible":
      return "ok";
    case "mismatch":
      return "error";
    default:
      return "warn";
  }
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
