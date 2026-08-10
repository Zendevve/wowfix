// First-run setup fallback: full-window (main.ts hides the sidebar). Shows
// only when auto-detection found nothing or the saved path is stale. A
// single path field + Browse (native folder picker via the Wails runtime,
// folder-input fallback). Completing the flow persists the install and
// reloads the shell, which re-reads state and routes into the app.

import type { View } from "../view";
import type { Install } from "../types";
import { service, mockActive } from "../api";
import { icon } from "../icons";
import { toast } from "../toast";
import "./setup.css";

// Manual path typing: derive the client flavor from a known folder segment.
// Order matters — the longer segments contain `_classic_` as a substring.
const FLAVOR_SEGMENTS: { segment: string; flavor: string }[] = [
  { segment: "_classic_era_", flavor: "_classic_era_" },
  { segment: "_classic_tbc_", flavor: "_classic_tbc_" },
  { segment: "_retail_", flavor: "_retail_" },
  { segment: "_classic_", flavor: "_classic_" },
];

function deriveFlavorFromPath(path: string): string | null {
  const segments = path.split(/[\\/]/);
  for (const { segment, flavor } of FLAVOR_SEGMENTS) {
    if (segments.some((seg) => seg.includes(segment))) return flavor;
  }
  return null;
}

// Only one view mounts at a time; a single flag keeps post-await renders
// from clobbering the host after the shell has unmounted us.
let disposed: { gone: boolean } | null = null;

export const view: View = {
  id: "setup",
  label: "Setup",
  icon: "shield",
  async mount(host) {
    disposed = { gone: false };
    const isGone = () => disposed?.gone ?? false;
    const local = {
      root: "",
      flavor: null as string | null,
      busy: false,
      error: null as string | null,
    };

    const render = (): void => {
      host.innerHTML = `
        <main class="setup-page">
          <header class="setup-hero">
            <p class="setup-kicker">wowfix — first run</p>
            <h1 class="setup-title">Repair your addons.</h1>
            <p class="setup-sub">Tell wowfix where your World of Warcraft lives and it will scan
              and repair your addons — every change is backed up, removals go to the OS trash.</p>
          </header>

          <section class="setup-form" aria-label="Manual install entry">
            <div class="setup-form-head">
              <h2 class="setup-form-title">Point us at your install</h2>
              <p class="setup-form-sub">The folder that contains <span class="mono">Interface/AddOns</span> for the client you play.</p>
            </div>

            <div class="setup-field">
              <label class="setup-label" for="setup-root">World of Warcraft install</label>
              <div class="setup-path-row">
                <input id="setup-root" class="text-input setup-root-input" type="text"
                  spellcheck="false" autocomplete="off" placeholder="C:\\Games\\World of Warcraft"
                  value="${escapeAttr(local.root)}" aria-describedby="setup-root-hint" />
                <button type="button" class="btn-secondary setup-browse" data-browse ${local.busy ? "disabled" : ""}>
                  ${icon("folder", 15)}<span>Browse…</span>
                </button>
              </div>
              <p class="setup-hint" id="setup-root-hint">The client flavor is derived from the path when possible.</p>
            </div>

            ${local.error ? `<p class="setup-error" role="alert">${icon("x-circle", 14)}<span>${escapeHtml(local.error)}</span></p>` : ""}

            <button class="btn-primary setup-cta" data-continue ${local.busy ? "disabled" : ""}>
              ${
                local.busy
                  ? `<span class="setup-spinner"></span><span>Setting up…</span>`
                  : `${icon("check", 16)}<span>Start using wowfix</span>`
              }
            </button>
          </section>
        </main>`;

      const rootInput = host.querySelector<HTMLInputElement>("#setup-root")!;

      rootInput.addEventListener("input", () => {
        local.root = rootInput.value.trim();
        const derived = deriveFlavorFromPath(local.root);
        if (derived) local.flavor = derived;
        local.error = null;
      });

      host.querySelector<HTMLElement>("[data-browse]")?.addEventListener("click", async () => {
        local.error = null;
        try {
          const picked = await pickFolder();
          if (picked) {
            local.root = picked;
            const derived = deriveFlavorFromPath(picked);
            if (derived) local.flavor = derived;
            if (!isGone()) {
              render();
              host.querySelector<HTMLInputElement>("#setup-root")?.focus();
            }
          }
        } catch (err) {
          local.error = errText(err, "Could not open the folder picker");
          if (!isGone()) render();
        }
      });

      host.querySelector<HTMLElement>("[data-continue]")?.addEventListener("click", async () => {
        if (!local.root) {
          local.error = "Enter the path to your World of Warcraft install.";
          render();
          return;
        }
        local.busy = true;
        local.error = null;
        render();
        try {
          // The backend detects the game version from the client exe; the
          // flavor only narrows the client folder when the path gives it away.
          // Persist the detected profile too, so cfg.profile is not left stale.
          const installed: Install = await service.SetInstall(local.root, local.flavor ?? "");
          if (installed.profile_id) await service.SetProfile(installed.profile_id);
          // The shell reads state once at boot; after the install is
          // persisted, reloading hands control back to it cleanly.
          history.replaceState(null, "", mockActive ? "?mock=1" : "?view=overview");
          window.location.reload();
        } catch (err) {
          local.busy = false;
          local.error = errText(err, "Could not set up this install");
          toast({ type: "error", title: "Setup failed", message: local.error });
          render();
        }
      });
    };

    render();
    const rootInput = host.querySelector<HTMLInputElement>("#setup-root");
    rootInput?.focus();
    rootInput?.setSelectionRange(rootInput.value.length, rootInput.value.length);

    // A stale saved path (auto-detection no longer finds it) is prefilled
    // so the user can re-validate or replace it.
    try {
      const state = await service.GetState();
      if (!isGone() && state.wow_path && !local.root) {
        local.root = state.wow_path;
        const derived = deriveFlavorFromPath(local.root);
        if (derived) local.flavor = derived;
        render();
      }
    } catch (err) {
      if (isGone()) return;
      local.error = errText(err, "Could not read the saved install path");
      render();
    }
  },
  unmount() {
    if (disposed) disposed.gone = true;
  },
};

/** Native folder picker when the Wails runtime is present; folder-input
 *  fallback otherwise (returns the selected folder's name — the browser
 *  never exposes absolute paths). */
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

function errText(err: unknown, fallback: string): string {
  if (err instanceof Error && err.message) return err.message;
  return String(err ?? fallback);
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
