const byId = (id) => document.getElementById(id);

let startedAt = null;
let readyProviders = [];
let launchRole = null;

function formatUptime() {
  if (!startedAt) return "—";
  const seconds = Math.max(0, Math.floor((Date.now() - startedAt.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

async function requestJSON(path, options = {}) {
  const response = await fetch(path, { cache: "no-store", ...options });
  if (!response.ok) {
    let detail = `${response.status} ${response.statusText}`;
    try {
      const body = await response.json();
      if (body.error) detail = body.error;
    } catch (_) {
      // The status remains sufficient when an intermediary returns non-JSON.
    }
    throw new Error(detail);
  }
  return response.json();
}

function setNodeState(status, label) {
  byId("node-pulse").className = status === "ready" ? "ready" : "error";
  byId("node-status").textContent = label;
}

async function loadHealth() {
  try {
    const health = await requestJSON("/api/v1/health");
    startedAt = new Date(health.startedAt);
    byId("node-id").textContent = health.nodeId;
    byId("side-node").textContent = health.nodeId;
    byId("node-subtitle").textContent = `${health.nodeId} · local execution node`;
    byId("node-version").textContent = health.version;
    byId("node-uptime").textContent = formatUptime();
    setNodeState("ready", "Live · local");
  } catch (error) {
    setNodeState("error", "Node unavailable");
  }
}

function setMetric(prefix, ready, total) {
  byId(`${prefix}-ready`).textContent = String(ready);
  byId(`${prefix}-meter`).style.width = `${total === 0 ? 0 : (ready / total) * 100}%`;
}

function badge(status) {
  const element = document.createElement("span");
  const tone = status === "ready" ? "ok" : ["missing", "drifted", "failed"].includes(status) ? "danger" : "warn";
  element.className = `ph-badge ${tone}`;
  element.textContent = status;
  return element;
}

function renderProfiles(snapshot) {
  const tbody = byId("profiles-body");
  tbody.replaceChildren();
  for (const provider of snapshot.providers) {
    const row = document.createElement("tr");
    const name = document.createElement("td");
    const nameBlock = document.createElement("div");
    nameBlock.className = "ph-name";
    const strong = document.createElement("strong");
    strong.textContent = provider.label;
    const small = document.createElement("small");
    small.textContent = provider.id;
    nameBlock.append(strong, small);
    name.append(nameBlock);

    const executable = document.createElement("td");
    executable.append(badge(provider.executable.status));
    const kit = document.createElement("td");
    kit.append(badge(provider.skillKit.status));
    const mcp = document.createElement("td");
    mcp.className = "ph-cell-check";
    const mcpDetail = document.createElement("small");
    mcpDetail.textContent = provider.mcp.detail;
    if (provider.mcp.action) mcp.title = provider.mcp.action;
    mcp.append(badge(provider.mcp.status), mcpDetail);
    const roles = document.createElement("td");
    roles.className = "ph-detail";
    roles.textContent = provider.roles.join(" · ");
    const readiness = document.createElement("td");
    readiness.append(badge(provider.status));
    row.append(name, executable, kit, mcp, roles, readiness);
    tbody.append(row);
  }

  const kitBadge = byId("kit-badge");
  kitBadge.className = `ph-badge ${snapshot.kit.status === "ready" ? "ok" : "danger"}`;
  kitBadge.textContent = snapshot.kit.status;
  const revision = snapshot.kit.revision ? snapshot.kit.revision.slice(0, 12) : "unknown";
  byId("kit-summary").textContent = `${snapshot.kit.detail} · Snowcat ${revision}`;

  readyProviders = snapshot.providers.filter((provider) => provider.status === "ready");
  const readyCount = readyProviders.length;
  const preflightCount = snapshot.providers.filter((provider) => provider.status === "preflight-required").length;
  const label = readyCount
    ? `${readyCount} ${readyCount === 1 ? "provider" : "providers"} ready`
    : preflightCount
      ? "preflight required"
      : "profile blocked";
  for (const id of ["discoverer-profile-state", "implementer-profile-state", "reviewer-profile-state"]) {
    const element = byId(id);
    element.textContent = label;
    element.className = `ph-badge ${readyCount ? "ok" : preflightCount ? "warn" : "danger"}`;
  }
  byId("launch-discoverer").disabled = readyCount === 0;
  byId("launch-implementer").disabled = readyCount === 0;
  byId("launch-reviewer").disabled = readyCount === 0;
}

function workerAction(label, tone, action) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `ph-button${tone === "secondary" ? " secondary" : ""}`;
  button.textContent = label;
  button.addEventListener("click", action);
  return button;
}

async function mutateWorker(workerId, action) {
  const cleanup = action === "cleanup";
  if (cleanup && !window.confirm(`Clean workspace for ${workerId}? The local branch is retained.`)) return;
  const path = cleanup ? `/api/v1/workers/${workerId}` : `/api/v1/workers/${workerId}/stop`;
  try {
    await requestJSON(path, { method: cleanup ? "DELETE" : "POST" });
    await loadWorkers();
  } catch (error) {
    byId("workers-badge").className = "ph-badge danger";
    byId("workers-badge").textContent = "action failed";
    byId("workers-summary").textContent = error.message;
  }
}

async function openWorkerConsole(workerId) {
  const terminal = window.open("about:blank", `snowcat-console-${workerId}`);
  try {
    const consoleInfo = await requestJSON(`/api/v1/workers/${workerId}/console`, { method: "POST" });
    if (terminal) {
      terminal.opener = null;
      terminal.location.replace(consoleInfo.url);
    } else {
      window.open(consoleInfo.url, "_blank", "noopener");
    }
  } catch (error) {
    if (terminal) terminal.close();
    byId("workers-badge").className = "ph-badge danger";
    byId("workers-badge").textContent = "console failed";
    byId("workers-summary").textContent = error.message;
  }
}

function renderWorkers(records) {
  const tbody = byId("workers-body");
  tbody.replaceChildren();
  if (records.length === 0) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 7;
    cell.className = "ph-empty";
    cell.textContent = "No managed workers yet. Launch one bounded worker above.";
    row.append(cell);
    tbody.append(row);
  }
  for (const worker of records) {
    const row = document.createElement("tr");
    const identity = document.createElement("td");
    const name = document.createElement("div");
    name.className = "ph-name";
    const strong = document.createElement("strong");
    strong.textContent = worker.id;
    const small = document.createElement("small");
    small.textContent = worker.branch;
    name.append(strong, small);
    identity.append(name);

    const state = document.createElement("td");
    state.append(badge(worker.status));
    const provider = document.createElement("td");
    provider.textContent = worker.provider;
    const role = document.createElement("td");
    role.textContent = worker.role;
    const repository = document.createElement("td");
    repository.textContent = worker.repository;
    const workspace = document.createElement("td");
    const workspacePath = document.createElement("span");
    workspacePath.className = "ph-workspace";
    workspacePath.title = worker.workspace;
    workspacePath.textContent = worker.workspace;
    workspace.append(workspacePath);
    const actions = document.createElement("td");
    actions.className = "ph-actions";
    if (["running", "exited", "failed"].includes(worker.status)) {
      actions.append(workerAction("Open terminal", "primary", () => openWorkerConsole(worker.id)));
      actions.append(workerAction("Stop", "secondary", () => mutateWorker(worker.id, "stop")));
    }
    if (["exited", "failed", "stopped"].includes(worker.status)) {
      actions.append(workerAction("Cleanup", "secondary", () => mutateWorker(worker.id, "cleanup")));
    }
    if (worker.status === "cleaned") {
      const retained = document.createElement("small");
      retained.textContent = "record retained";
      actions.append(retained);
    }
    row.append(identity, state, provider, role, repository, workspace, actions);
    tbody.append(row);
  }
  const active = records.filter((worker) => ["running", "allocating"].includes(worker.status)).length;
  const retained = records.filter((worker) => worker.status !== "cleaned").length;
  const workersBadge = byId("workers-badge");
  workersBadge.className = `ph-badge ${active ? "ok" : ""}`;
  workersBadge.textContent = `${active} active`;
  byId("workers-summary").textContent = retained
    ? `${retained} retained ${retained === 1 ? "workspace" : "workspaces"} · explicit cleanup only`
    : "No retained workspaces";
}

async function loadWorkers() {
  try {
    renderWorkers(await requestJSON("/api/v1/workers"));
  } catch (error) {
    byId("workers-badge").className = "ph-badge danger";
    byId("workers-badge").textContent = "error";
    byId("workers-summary").textContent = error.message;
  }
}

function openLaunchDialog(role) {
  launchRole = role;
  byId("launch-role").value = role;
  byId("launch-title").textContent = `Launch one ${role}`;
  byId("launch-message").textContent = "";
  const provider = byId("launch-provider");
  provider.replaceChildren();
  for (const candidate of readyProviders) {
    const option = document.createElement("option");
    option.value = candidate.id;
    option.textContent = candidate.label;
    provider.append(option);
  }
  byId("launch-dialog").showModal();
}

function closeLaunchDialog() {
  if (byId("launch-dialog").open) byId("launch-dialog").close();
}

async function submitLaunch(event) {
  event.preventDefault();
  const submit = byId("launch-submit");
  submit.disabled = true;
  byId("launch-message").textContent = "Allocating worktree and retained terminal…";
  try {
    const record = await requestJSON("/api/v1/workers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        provider: byId("launch-provider").value,
        role: launchRole,
        repository: byId("launch-repository").value.trim(),
        source: byId("launch-source").value.trim(),
        baseRef: byId("launch-base-ref").value.trim(),
      }),
    });
    closeLaunchDialog();
    await loadWorkers();
    byId("workers-summary").textContent = `${record.id} launched · terminal and workspace retained`;
    byId("workers").scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (error) {
    byId("launch-message").textContent = error.message;
  } finally {
    submit.disabled = false;
  }
}

function renderChecks(result) {
  const tbody = byId("checks");
  tbody.replaceChildren();

  for (const check of result.checks) {
    const row = document.createElement("tr");
    const capability = document.createElement("td");
    const name = document.createElement("div");
    name.className = "ph-name";
    const strong = document.createElement("strong");
    strong.textContent = check.name;
    const small = document.createElement("small");
    small.textContent = check.status === "ready" ? "available on this node" : "not detected";
    name.append(strong, small);
    capability.append(name);

    const category = document.createElement("td");
    category.textContent = check.category;
    const requirement = document.createElement("td");
    const requirementBadge = document.createElement("span");
    requirementBadge.className = "ph-badge";
    requirementBadge.textContent = check.required ? "required" : "optional";
    requirement.append(requirementBadge);
    const state = document.createElement("td");
    const stateBadge = document.createElement("span");
    stateBadge.className = `ph-badge ${check.status === "ready" ? "ok" : check.required ? "danger" : "warn"}`;
    stateBadge.textContent = check.status;
    state.append(stateBadge);
    const detail = document.createElement("td");
    detail.className = "ph-detail";
    detail.textContent = check.action || check.detail;
    row.append(capability, category, requirement, state, detail);
    tbody.append(row);
  }

  const countReady = (names) => result.checks.filter((check) => names.includes(check.name) && check.status === "ready").length;
  const core = countReady(["git", "tmux"]);
  const providers = countReady(["codex", "claude", "copilot"]);
  const adapters = countReady(["podman", "docker"]);
  setMetric("core", core, 2);
  setMetric("provider", providers, 3);
  setMetric("adapter", adapters, 2);
  byId("core-state").textContent = result.status;
  byId("core-meter").parentElement.className = `ph-meter${result.status === "degraded" ? " danger" : ""}`;

  const optionalMissing = result.checks.filter((check) => !check.required && check.status !== "ready").length;
  const badge = byId("readiness-badge");
  badge.className = `ph-badge ${result.status === "degraded" ? "danger" : "ok"}`;
  badge.textContent = result.status;
  byId("readiness-summary").textContent = result.status === "degraded"
    ? "Required tooling needs attention"
    : optionalMissing
      ? `${optionalMissing} optional ${optionalMissing === 1 ? "capability is" : "capabilities are"} unavailable`
      : "Every detected capability is available";
}

async function loadChecks() {
  const button = byId("refresh");
  button.disabled = true;
  try {
    renderChecks(await requestJSON("/api/v1/doctor"));
  } catch (error) {
    const badge = byId("readiness-badge");
    badge.className = "ph-badge danger";
    badge.textContent = "error";
    byId("readiness-summary").textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

async function loadProfiles() {
  try {
    renderProfiles(await requestJSON("/api/v1/profiles"));
  } catch (error) {
    const kitBadge = byId("kit-badge");
    kitBadge.className = "ph-badge danger";
    kitBadge.textContent = "error";
    byId("kit-summary").textContent = error.message;
  }
}

byId("refresh").addEventListener("click", () => {
  loadHealth();
  loadChecks();
  loadProfiles();
  loadWorkers();
});
byId("launch-discoverer").addEventListener("click", () => openLaunchDialog("discoverer"));
byId("launch-implementer").addEventListener("click", () => openLaunchDialog("implementer"));
byId("launch-reviewer").addEventListener("click", () => openLaunchDialog("reviewer"));
byId("launch-close").addEventListener("click", closeLaunchDialog);
byId("launch-cancel").addEventListener("click", closeLaunchDialog);
byId("launch-form").addEventListener("submit", submitLaunch);
loadHealth();
loadChecks();
loadProfiles();
loadWorkers();
setInterval(() => { byId("node-uptime").textContent = formatUptime(); }, 1000);
setInterval(loadWorkers, 5000);
