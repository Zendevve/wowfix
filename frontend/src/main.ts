// App shell and bootstrap. Owns the sidebar, statusbar, ?view= router and
// the view registry. Views are mounted into #content and only touch their
// own files.

import "@fontsource-variable/inter";
import "@fontsource-variable/mona-sans";
import "./tokens.css";
import "./base.css";
import "./components.css";
import "./shell.css";

import { service, mockActive } from "./api";
import type { State } from "./types";
import type { View, ViewId } from "./view";
import { icon, type IconName } from "./icons";
import { mountToasts } from "./toast";
import { mountDialog } from "./dialog";
import { view as setupView } from "./views/setup";
import { view as overviewView } from "./views/overview";
import { view as catalogView } from "./views/catalog";
import { view as updatesView } from "./views/updates";
import { view as collectionsView } from "./views/collections";
import { view as backupsView } from "./views/backups";
import { view as savedvarsView } from "./views/savedvars";
import { view as settingsView } from "./views/settings";

const appEl = document.getElementById("app")!;
appEl.innerHTML = `
  <div class="app">
    <aside class="sidebar" id="sidebar" aria-label="Sidebar"></aside>
    <main class="view-frame"><div class="view-inner" id="content"></div></main>
    <footer class="statusbar" id="statusbar" aria-live="polite"></footer>
  </div>`;

const grid = appEl.querySelector<HTMLElement>(".app")!;
const sidebar = appEl.querySelector<HTMLElement>("#sidebar")!;
const content = appEl.querySelector<HTMLElement>("#content")!;
const statusbar = appEl.querySelector<HTMLElement>("#statusbar")!;

mountToasts(appEl);
mountDialog(appEl);

// View registry: all 8 destinations. Setup is full-window (no sidebar) and
// only reachable when no install is configured; the other seven are the
// sidebar destinations.
const VIEWS: Record<ViewId, View> = {
  setup: setupView,
  overview: overviewView,
  catalog: catalogView,
  updates: updatesView,
  collections: collectionsView,
  backups: backupsView,
  savedvars: savedvarsView,
  settings: settingsView,
};

// Sidebar order is taste, not contract.
const NAV: ViewId[] = [
  "overview",
  "updates",
  "catalog",
  "collections",
  "backups",
  "savedvars",
  "settings",
];

// Statusbar flavor labels for the active install. A root install ("") shows
// "Top-level"; unknown flavors drop the chip. The chip is only rendered when
// an install is configured, so the setup fallback screen shows none.
const FLAVOR_LABEL: Record<string, string> = {
  "": "Top-level",
  _retail_: "Retail",
  _classic_: "Wrath Classic",
  _classic_era_: "Classic Era",
  _classic_tbc_: "TBC Classic",
};

let app: { state: State; view: ViewId };
let current: View | null = null;

function viewIdFromQuery(): ViewId | null {
  const requested = new URLSearchParams(window.location.search).get("view");
  if (requested && NAV.includes(requested as ViewId)) {
    return requested as ViewId;
  }
  return null;
}

function renderSidebar(): void {
  const open = grid.classList.contains("sidebar-open");
  sidebar.innerHTML = `
    <div class="brand">
      ${icon("shield", 22)}
      <span class="brand-name">wowfix</span>
    </div>
    <nav class="nav" aria-label="Views">
      ${NAV.map((id) => {
        const v = VIEWS[id];
        return `
        <button class="nav-item${app.view === id ? " active" : ""}" data-view="${id}" ${app.view === id ? 'aria-current="page"' : ""} title="${escapeAttr(v.label)}">
          <span class="nav-glyph">${icon(v.icon as IconName, 18)}</span>
          <span class="nav-label">${escapeHtml(v.label)}</span>
        </button>`;
      }).join("")}
    </nav>
    <button class="sidebar-toggle" data-collapse aria-expanded="${open}" aria-controls="sidebar" title="${open ? "Collapse sidebar" : "Expand sidebar"}">
      <span class="nav-glyph">${icon(open ? "chevron-left" : "chevron-right", 16)}</span>
      <span class="nav-label">${open ? "Collapse" : ""}</span>
    </button>`;

  sidebar.querySelectorAll<HTMLButtonElement>(".nav-item").forEach((btn) => {
    btn.addEventListener("click", () => {
      go(btn.dataset.view as ViewId);
      btn.focus();
    });
  });
  sidebar.querySelector("[data-collapse]")!.addEventListener("click", () => {
    grid.classList.toggle("sidebar-open");
    renderSidebar();
  });
}

function renderStatus(): void {
  const backend = mockActive ? "mock" : `v${escapeHtml(app.state.version)}`;
  const flavor = app.state.has_install ? (FLAVOR_LABEL[app.state.flavor] ?? "") : "";
  statusbar.innerHTML = `
    <div class="statusbar-left">
      <span class="status-chip">${escapeHtml(VIEWS[app.view].label)}</span>
      ${flavor ? `<span class="status-chip">${escapeHtml(flavor)}</span>` : ""}
    </div>
    <div class="statusbar-right">
      ${mockActive ? `<span class="status-chip mock">MOCK</span>` : ""}
      <span class="status-chip mono">${backend}</span>
    </div>`;
}

function mountView(): void {
  current?.unmount?.();
  current = null;
  content.innerHTML = "";
  const view = VIEWS[app.view];
  const result = view.mount(content);
  if (result instanceof Promise) void result;
  current = view;
  content.classList.remove("view-enter");
  void content.offsetWidth;
  content.classList.add("view-enter");
}

function syncChrome(): void {
  grid.classList.toggle("no-sidebar", app.view === "setup");
  renderSidebar();
  renderStatus();
  mountView();
}

function go(viewId: ViewId): void {
  if (app.view === viewId) return;
  app.view = viewId;
  if (viewId !== "setup") {
    history.replaceState(null, "", `?view=${viewId}`);
  }
  syncChrome();
}

async function boot(): Promise<void> {
  try {
    const state = await service.GetState();
    const requested = viewIdFromQuery();
    app = {
      state,
      view: state.has_install ? (requested ?? "overview") : "setup",
    };
    syncChrome();
  } catch (err) {
    appEl.innerHTML = `<div class="fatal">${icon("x-circle", 26)}
      <h1>Could not start wowfix</h1>
      <p>${escapeHtml(errText(err, "The backend connection failed."))}</p></div>`;
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

void boot();
