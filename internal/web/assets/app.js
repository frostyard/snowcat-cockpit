const byId = (id) => document.getElementById(id);

let startedAt = null;
let readyProviders = [];
let launchRole = null;
let fleetRole = null;
let latestSnapshot = null;
let latestWorkers = [];
let latestRepositories = [];
let latestCampaign = null;
let observingActiveWorkers = false;
const workerObservations = new Map();

function formatUptime() {
  if (!startedAt) return "—";
  const seconds = Math.max(0, Math.floor((Date.now() - startedAt.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

function formatAge(value) {
  if (!value) return "waiting";
  const timestamp = new Date(value);
  if (Number.isNaN(timestamp.getTime())) return "unknown";
  const seconds = Math.max(0, Math.floor((Date.now() - timestamp.getTime()) / 1000));
  if (seconds < 5) return "now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  return `${Math.floor(minutes / 60)}h ago`;
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

function mcpServerFor(provider) {
  return provider === "copilot" ? "snowcat-mcp" : "snowcat";
}

function renderRepositories(records) {
  latestRepositories = records;
  const tbody = byId("repositories-body");
  tbody.replaceChildren();
  if (records.length === 0) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 4;
    cell.className = "ph-empty";
    cell.textContent = "Enroll a repository once; campaign start prepares its retained source.";
    row.append(cell);
    tbody.append(row);
  }
  for (const repository of records) {
    const row = document.createElement("tr");
    const identity = document.createElement("td");
    const name = document.createElement("div");
    name.className = "ph-name";
    const strong = document.createElement("strong");
    strong.textContent = repository.repository;
    const source = document.createElement("small");
    source.textContent = repository.source;
    source.title = repository.source;
    name.append(strong, source);
    identity.append(name);
    const base = document.createElement("td");
    base.textContent = repository.baseCommit ? repository.baseCommit.slice(0, 12) : "not prepared";
    base.title = repository.baseRef || "origin/HEAD";
    const state = document.createElement("td");
    const stateBadge = badge(repository.status);
    stateBadge.title = repository.detail;
    state.append(stateBadge);
    const actions = document.createElement("td");
    actions.className = "ph-actions";
    const setup = workerAction(repository.status === "ready" ? "Refresh" : "Prepare", "secondary", () => setupRepository(repository.repository));
    actions.append(setup);
    row.append(identity, base, state, actions);
    tbody.append(row);
  }
  byId("campaign-start").disabled = records.length === 0;
}

async function loadRepositories() {
  try {
    renderRepositories(await requestJSON("/api/v1/repositories"));
  } catch (error) {
    byId("campaign-message").textContent = error.message;
  }
}

async function enrollRepository(event) {
  event.preventDefault();
  const button = byId("repository-enroll");
  button.disabled = true;
  byId("campaign-message").textContent = "Adding retained local execution configuration…";
  try {
    await requestJSON("/api/v1/repositories", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ repository: byId("repository-slug").value.trim() }),
    });
    byId("repository-slug").value = "";
    byId("campaign-message").textContent = "Repository enrolled locally. Start prepares every source together.";
    await loadRepositories();
  } catch (error) {
    byId("campaign-message").textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

async function setupRepository(repository) {
  const [owner, name] = repository.split("/");
  byId("campaign-message").textContent = `Preparing ${repository} without deleting or resetting retained state…`;
  try {
    await requestJSON(`/api/v1/repositories/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/setup`, { method: "POST" });
    byId("campaign-message").textContent = `${repository} is prepared from its refreshed GitHub origin.`;
  } catch (error) {
    byId("campaign-message").textContent = error.message;
  }
  await loadRepositories();
}

function renderCampaignActivity(record) {
  const workerIds = record?.workerIds || [];
  const activeWorkers = latestWorkers.filter((worker) => ["running", "allocating"].includes(worker.status));
  const observations = (record?.repositories || [])
    .map((repository) => repository.observedAt)
    .filter(Boolean)
    .map((value) => new Date(value))
    .filter((value) => !Number.isNaN(value.getTime()) && value.getUTCFullYear() > 1);
  const lastObserved = observations.length
    ? new Date(Math.max(...observations.map((value) => value.getTime())))
    : null;

  byId("campaign-active-workers").textContent = String(activeWorkers.length);
  byId("campaign-launched-workers").textContent = String(workerIds.length);
  byId("campaign-last-observed").textContent = formatAge(lastObserved);
  byId("campaign-last-observed-at").textContent = lastObserved
    ? lastObserved.toLocaleString()
    : "waiting for observation";
}

function renderCampaign(record) {
  latestCampaign = record;
  const active = ["starting", "running", "degraded", "stopping"].includes(record.status);
  if (record.id && record.request) {
    byId("campaign-adapter").value = record.request.adapter || "host";
    byId("campaign-runtime").value = record.request.runtime || "podman";
    if (record.request.discoverer?.provider === record.request.implementer?.provider) {
      byId("campaign-work-provider").value = record.request.discoverer.provider;
    }
    const reviewProvider = byId("campaign-review-provider");
    if (Array.from(reviewProvider.options).some((option) => option.value === record.request.reviewer?.provider)) {
      reviewProvider.value = record.request.reviewer.provider;
    }
    if (record.request.discoverer?.capacity) byId("campaign-discoverers").value = String(record.request.discoverer.capacity);
    if (record.request.implementer?.capacity) byId("campaign-implementers").value = String(record.request.implementer.capacity);
    if (record.request.reviewer?.capacity) byId("campaign-reviewers").value = String(record.request.reviewer.capacity);
  }
  const campaignBadge = byId("campaign-badge");
  campaignBadge.className = `ph-badge ${record.status === "running" ? "ok" : record.status === "degraded" ? "danger" : "warn"}`;
  campaignBadge.textContent = record.status;
  byId("campaign-summary").textContent = record.detail;
  renderCampaignActivity(record);
  byId("campaign-start").disabled = active || latestRepositories.length === 0;
  byId("campaign-stop").disabled = !active || record.status === "stopping";
  for (const id of ["campaign-adapter", "campaign-runtime", "campaign-work-provider", "campaign-review-provider", "campaign-discoverers", "campaign-implementers", "campaign-reviewers"]) {
    byId(id).disabled = active || (id === "campaign-runtime" && byId("campaign-adapter").value !== "oci");
  }

  const detail = byId("campaign-detail");
  detail.replaceChildren();
  for (const provider of record.providers || []) {
    const card = document.createElement("div");
    const name = document.createElement("strong");
    name.textContent = `${provider.provider} · ${provider.mcpServer}`;
    const status = document.createElement("small");
    status.textContent = `${provider.status} · ${provider.detail}`;
    card.append(name, status);
    detail.append(card);
  }
  for (const repository of record.repositories || []) {
    const card = document.createElement("div");
    const name = document.createElement("strong");
    name.textContent = repository.repository;
    const status = document.createElement("small");
    const commit = repository.baseCommit ? ` · ${repository.baseCommit.slice(0, 12)}` : "";
    status.textContent = `${repository.status}${commit} · ${repository.detail}`;
    card.append(name, status);
    detail.append(card);
  }
}

async function loadCampaign() {
  try {
    renderCampaign(await requestJSON("/api/v1/campaign"));
  } catch (error) {
    const campaignBadge = byId("campaign-badge");
    campaignBadge.className = "ph-badge danger";
    campaignBadge.textContent = "unavailable";
    byId("campaign-summary").textContent = error.message;
  }
}

async function startCampaign() {
  const button = byId("campaign-start");
  button.disabled = true;
  byId("campaign-message").textContent = "Starting setup and bounded provider preflight for every enrolled repository…";
  const workProvider = byId("campaign-work-provider").value;
  const reviewProvider = byId("campaign-review-provider").value;
  try {
    const record = await requestJSON("/api/v1/campaign", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        adapter: byId("campaign-adapter").value,
        runtime: byId("campaign-adapter").value === "oci" ? byId("campaign-runtime").value : "",
        intervalSeconds: 30,
        discoverer: { provider: workProvider, mcpServer: mcpServerFor(workProvider), capacity: Number(byId("campaign-discoverers").value) },
        implementer: { provider: workProvider, mcpServer: mcpServerFor(workProvider), capacity: Number(byId("campaign-implementers").value) },
        reviewer: { provider: reviewProvider, mcpServer: mcpServerFor(reviewProvider), capacity: Number(byId("campaign-reviewers").value) },
      }),
    });
    renderCampaign(record);
    byId("campaign-message").textContent = "Campaign accepted. Setup and preflight continue in the node.";
  } catch (error) {
    byId("campaign-message").textContent = error.message;
    button.disabled = latestRepositories.length === 0;
  }
}

async function stopCampaign() {
  byId("campaign-stop").disabled = true;
  byId("campaign-message").textContent = "Stopping future launches; existing workers and workspaces remain retained…";
  try {
    renderCampaign(await requestJSON("/api/v1/campaign/stop", { method: "POST" }));
  } catch (error) {
    byId("campaign-message").textContent = error.message;
  }
}

function syncCampaignRuntime() {
  byId("campaign-runtime").disabled = byId("campaign-adapter").value !== "oci";
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
  updateFleetButtons();
}

function updateFleetButtons() {
  for (const role of ["discoverer", "implementer", "reviewer"]) {
    const eligible = latestSnapshot?.counts?.[role] || 0;
    const button = byId(`launch-fleet-${role}`);
    button.disabled = readyProviders.length === 0 || eligible === 0;
    button.title = latestSnapshot
      ? `${eligible} eligible ${eligible === 1 ? "item" : "items"} in the latest snapshot`
      : "Observe the queue before planning a fleet";
  }
}

function renderQueue(snapshot) {
  latestSnapshot = snapshot;
  for (const role of ["discoverer", "implementer", "reviewer", "unassigned"]) {
    byId(`queue-${role}-count`).textContent = String(snapshot.counts?.[role] || 0);
  }
  const observedAt = new Date(snapshot.observedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  const flagged = snapshot.flagged ? ` · ${snapshot.flagged} contract ${snapshot.flagged === 1 ? "warning" : "warnings"} withheld` : "";
  byId("queue-summary").textContent = `${snapshot.items.length} claimable at ${observedAt}${snapshot.truncated ? " · one projection truncated at 100" : " · complete bounded response"}${flagged}`;
  byId("queue-repository").value = snapshot.repository;

  const tbody = byId("queue-body");
  tbody.replaceChildren();
  if (snapshot.items.length === 0) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 5;
    cell.className = "ph-empty";
    cell.textContent = `No claimable work observed for ${snapshot.repository}.`;
    row.append(cell);
    tbody.append(row);
  }
  for (const item of snapshot.items) {
    const row = document.createElement("tr");
    const kind = document.createElement("td");
    const kindName = document.createElement("div");
    kindName.className = "ph-name";
    const strong = document.createElement("strong");
    strong.textContent = item.kind;
    const small = document.createElement("small");
    small.textContent = item.id;
    kindName.append(strong, small);
    kind.append(kindName);
    const role = document.createElement("td");
    const roleBadge = badge(item.role === "unassigned" ? "unassigned" : "ready");
    roleBadge.textContent = item.role;
    if (item.role === "unassigned") roleBadge.className = "ph-badge warn";
    role.append(roleBadge);
    const priority = document.createElement("td");
    priority.textContent = String(item.priority);
    const artifact = document.createElement("td");
    const artifactName = document.createElement("div");
    artifactName.className = "ph-name";
    const artifactStrong = document.createElement("strong");
    artifactStrong.textContent = item.requiredArtifact || "unspecified";
    const contractBadge = badge(item.contract === "ready" ? "ready" : item.contract === "suspicious" ? "failed" : "unknown");
    contractBadge.textContent = item.contract;
    if (item.contractDetail) contractBadge.title = item.contractDetail;
    artifactName.append(artifactStrong, contractBadge);
    artifact.append(artifactName);
    const actions = document.createElement("td");
    actions.className = "ph-detail";
    actions.textContent = item.allowedActions.join(" · ");
    row.append(kind, role, priority, artifact, actions);
    tbody.append(row);
  }
  updateFleetButtons();
}

async function observeQueue(event) {
  event.preventDefault();
  const button = byId("queue-observe");
  button.disabled = true;
  byId("queue-summary").textContent = "Taking one bounded Snowcat snapshot…";
  try {
    const snapshot = await requestJSON("/api/v1/queue/snapshot", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ repository: byId("queue-repository").value.trim() }),
    });
    renderQueue(snapshot);
  } catch (error) {
    byId("queue-summary").textContent = error.message;
  } finally {
    button.disabled = false;
  }
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

async function observeWorker(workerId) {
  workerObservations.set(workerId, {
    status: "checking",
    detail: "Taking one exact Snowcat observation…",
  });
  renderWorkers(latestWorkers);
  try {
    const observation = await requestJSON(`/api/v1/workers/${workerId}/observe`, { method: "POST" });
    workerObservations.set(workerId, observation);
  } catch (error) {
    workerObservations.set(workerId, { status: "error", detail: error.message });
  }
  renderWorkers(latestWorkers);
}

async function observeActiveWorkers() {
  const button = byId("workers-observe-active");
  const activeWorkers = latestWorkers.filter((worker) => ["running", "allocating"].includes(worker.status));
  observingActiveWorkers = true;
  button.disabled = true;
  for (const worker of activeWorkers) {
    workerObservations.set(worker.id, {
      status: "checking",
      detail: "Taking one exact Snowcat observation…",
    });
  }
  renderWorkers(latestWorkers);
  await Promise.all(activeWorkers.map(async (worker) => {
    try {
      const observation = await requestJSON(`/api/v1/workers/${worker.id}/observe`, { method: "POST" });
      workerObservations.set(worker.id, observation);
    } catch (error) {
      workerObservations.set(worker.id, { status: "error", detail: error.message });
    }
  }));
  observingActiveWorkers = false;
  renderWorkers(latestWorkers);
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
    const observation = workerObservations.get(worker.id);
    const leaseConflict = ["running", "allocating"].includes(worker.status) && observation?.status === "expired";
    if (leaseConflict) row.className = "ph-worker-lease-conflict";
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
    const processBadge = badge(worker.status);
    if (leaseConflict) processBadge.className = "ph-badge danger";
    state.append(processBadge);
    if (observation) {
      const workState = document.createElement("small");
      workState.className = `ph-work-state${leaseConflict ? " danger" : ""}`;
      const item = observation.itemId ? ` · ${observation.itemId.slice(0, 8)}` : "";
      workState.textContent = leaseConflict
        ? `lease expired · process still running${item}`
        : `work ${observation.status}${item}`;
      workState.title = observation.detail;
      state.append(workState);
    }
    const provider = document.createElement("td");
    const providerName = document.createElement("div");
    providerName.className = "ph-name";
    const providerLabel = document.createElement("strong");
    providerLabel.textContent = worker.provider;
    const adapterLabel = document.createElement("small");
    const execution = [worker.adapter || "host"];
    if (worker.runtime) execution.push(`${worker.runtime} ${worker.runtimePosture || "unknown"}`);
    if (worker.model) execution.push(worker.model);
    adapterLabel.textContent = execution.join(" · ");
    providerName.append(providerLabel, adapterLabel);
    provider.append(providerName);
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
    const observe = workerAction("Observe work", "secondary", () => observeWorker(worker.id));
    observe.disabled = observation?.status === "checking";
    actions.append(observe);
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
  const leaseConflicts = records.filter((worker) =>
    ["running", "allocating"].includes(worker.status) && workerObservations.get(worker.id)?.status === "expired").length;
  const retained = records.filter((worker) => worker.status !== "cleaned").length;
  const workersBadge = byId("workers-badge");
  workersBadge.className = `ph-badge ${leaseConflicts ? "danger" : active ? "ok" : ""}`;
  workersBadge.textContent = leaseConflicts ? `${leaseConflicts} lease lost` : `${active} active`;
  byId("workers-observe-active").disabled = active === 0 || observingActiveWorkers;
  byId("workers-summary").textContent = retained
    ? `${retained} retained ${retained === 1 ? "workspace" : "workspaces"}${leaseConflicts ? ` · ${leaseConflicts} running without an active lease` : ""} · explicit cleanup only`
    : "No retained workspaces";
}

async function loadWorkers() {
  try {
    latestWorkers = await requestJSON("/api/v1/workers");
    renderWorkers(latestWorkers);
    if (latestCampaign) renderCampaignActivity(latestCampaign);
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
  populateProviders("launch");
  syncRuntime("launch");
  byId("launch-dialog").showModal();
}

function populateProviders(prefix) {
  const provider = byId(`${prefix}-provider`);
  provider.replaceChildren();
  const adapter = byId(`${prefix}-adapter`).value;
  const candidates = adapter === "oci" ? readyProviders.filter((candidate) => ["codex", "claude", "copilot"].includes(candidate.id)) : readyProviders;
  for (const candidate of candidates) {
    const option = document.createElement("option");
    option.value = candidate.id;
    option.textContent = candidate.label;
    provider.append(option);
  }
}

function syncRuntime(prefix) {
  const runtime = byId(`${prefix}-runtime`);
  runtime.disabled = byId(`${prefix}-adapter`).value !== "oci";
}

function closeLaunchDialog() {
  if (byId("launch-dialog").open) byId("launch-dialog").close();
}

function openFleetDialog(role) {
  fleetRole = role;
  const eligible = latestSnapshot?.counts?.[role] || 0;
  byId("fleet-title").textContent = `Launch ${role} fleet`;
  byId("fleet-message").textContent = `${eligible} eligible in the latest view; Cockpit will take a fresh snapshot before launch.`;
  populateProviders("fleet");
  syncRuntime("fleet");
  byId("fleet-count").value = String(Math.min(3, Math.max(1, eligible)));
  byId("fleet-repository").value = latestSnapshot?.repository || byId("queue-repository").value.trim();
  if (!byId("fleet-source").value) byId("fleet-source").value = byId("launch-source").value;
  byId("fleet-dialog").showModal();
}

function closeFleetDialog() {
  if (byId("fleet-dialog").open) byId("fleet-dialog").close();
}

async function submitFleet(event) {
  event.preventDefault();
  const submit = byId("fleet-submit");
  submit.disabled = true;
  byId("fleet-message").textContent = "Observing once, then allocating the bounded batch…";
  try {
    if (!(await confirmBase("fleet"))) return;
    const result = await requestJSON("/api/v1/fleets", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        adapter: byId("fleet-adapter").value,
        runtime: byId("fleet-adapter").value === "oci" ? byId("fleet-runtime").value : "",
        provider: byId("fleet-provider").value,
        mcpServer: mcpServerFor(byId("fleet-provider").value),
        role: fleetRole,
        repository: byId("fleet-repository").value.trim(),
        source: byId("fleet-source").value.trim(),
        baseRef: byId("fleet-base-ref").value.trim(),
        count: Number(byId("fleet-count").value),
      }),
    });
    renderQueue(result.snapshot);
    closeFleetDialog();
    await loadWorkers();
    const suffix = result.failures.length
      ? ` · stopped after launch ${result.failures[0].ordinal} failed`
      : result.planned < result.requested
        ? ` · capped from ${result.requested} to ${result.planned} eligible`
        : "";
    byId("workers-summary").textContent = `${result.launched.length} ${fleetRole} ${result.launched.length === 1 ? "worker" : "workers"} launched${suffix}`;
    byId("workers").scrollIntoView({ behavior: "smooth", block: "start" });
  } catch (error) {
    byId("fleet-message").textContent = error.message;
  } finally {
    submit.disabled = false;
  }
}

async function submitLaunch(event) {
  event.preventDefault();
  const submit = byId("launch-submit");
  submit.disabled = true;
  byId("launch-message").textContent = "Allocating worktree and retained terminal…";
  try {
    if (!(await confirmBase("launch"))) return;
    const record = await requestJSON("/api/v1/workers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        adapter: byId("launch-adapter").value,
        runtime: byId("launch-adapter").value === "oci" ? byId("launch-runtime").value : "",
        provider: byId("launch-provider").value,
        mcpServer: mcpServerFor(byId("launch-provider").value),
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

async function confirmBase(prefix) {
  const inspection = await requestJSON("/api/v1/workers/base", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      source: byId(`${prefix}-source`).value.trim(),
      baseRef: byId(`${prefix}-base-ref`).value.trim(),
    }),
  });
  const shortCommit = inspection.baseCommit.slice(0, 12);
  byId(`${prefix}-message`).textContent = `${inspection.detail} · selected ${shortCommit}`;
  if (!["behind", "diverged"].includes(inspection.status)) return true;
  return window.confirm(`${inspection.detail}\n\nLaunch from ${shortCommit} anyway?`);
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
  loadRepositories();
  loadCampaign();
});
byId("repository-form").addEventListener("submit", enrollRepository);
byId("campaign-start").addEventListener("click", startCampaign);
byId("campaign-stop").addEventListener("click", stopCampaign);
byId("campaign-adapter").addEventListener("change", syncCampaignRuntime);
byId("launch-discoverer").addEventListener("click", () => openLaunchDialog("discoverer"));
byId("launch-implementer").addEventListener("click", () => openLaunchDialog("implementer"));
byId("launch-reviewer").addEventListener("click", () => openLaunchDialog("reviewer"));
byId("launch-fleet-discoverer").addEventListener("click", () => openFleetDialog("discoverer"));
byId("launch-fleet-implementer").addEventListener("click", () => openFleetDialog("implementer"));
byId("launch-fleet-reviewer").addEventListener("click", () => openFleetDialog("reviewer"));
byId("launch-close").addEventListener("click", closeLaunchDialog);
byId("launch-cancel").addEventListener("click", closeLaunchDialog);
byId("launch-form").addEventListener("submit", submitLaunch);
byId("launch-adapter").addEventListener("change", () => {
  populateProviders("launch");
  syncRuntime("launch");
});
byId("fleet-close").addEventListener("click", closeFleetDialog);
byId("fleet-cancel").addEventListener("click", closeFleetDialog);
byId("fleet-form").addEventListener("submit", submitFleet);
byId("fleet-adapter").addEventListener("change", () => {
  populateProviders("fleet");
  syncRuntime("fleet");
});
byId("queue-form").addEventListener("submit", observeQueue);
byId("workers-observe-active").addEventListener("click", observeActiveWorkers);
loadHealth();
loadChecks();
loadProfiles();
loadWorkers();
loadRepositories();
loadCampaign();
setInterval(() => {
  byId("node-uptime").textContent = formatUptime();
  if (latestCampaign) renderCampaignActivity(latestCampaign);
}, 1000);
setInterval(() => { loadWorkers(); loadCampaign(); }, 5000);
