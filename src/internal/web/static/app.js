const tokenInput = document.querySelector("#token");
const message = document.querySelector("#message");
const endpoint = document.querySelector("#endpoint");

const state = {
  token: sessionStorage.getItem("qindexer.admin.token") || "",
  roots: [],
  editingRoot: null,
  creatingRoot: false,
  crawler: {},
  network: {},
};

let liveMetricsTimer = null;
let liveDashboardTimer = null;
let liveDashboardRefreshing = false;

tokenInput.value = state.token;
endpoint.textContent = `${location.origin}/admin/v1`;

document.querySelector("#save-token").addEventListener("click", () => {
  state.token = tokenInput.value.trim();
  sessionStorage.setItem("qindexer.admin.token", state.token);
  refresh();
});

document.querySelector("#clear-token").addEventListener("click", () => {
  state.token = "";
  tokenInput.value = "";
  sessionStorage.removeItem("qindexer.admin.token");
  refresh({ quiet: true, message: "Admin token cleared" });
});

document.querySelector("#refresh").addEventListener("click", refresh);
document.querySelector("#refresh-logs").addEventListener("click", refreshLogs);
document.querySelector("#validate-config").addEventListener("click", validateConfig);
document.querySelector("#pause-crawler").addEventListener("click", pauseCrawler);
document.querySelector("#stop-crawler").addEventListener("click", stopCrawler);
document.querySelector("#resume-crawler").addEventListener("click", resumeCrawler);
document.querySelector("#stop-service").addEventListener("click", stopService);
document.querySelector("#view-roots").addEventListener("click", () => showView("roots"));
document.querySelector("#view-config").addEventListener("click", () => showView("config"));
document.querySelector("#close-rules").addEventListener("click", closeRules);
document.querySelector("#cancel-rules").addEventListener("click", closeRules);
document.querySelector("#close-validation").addEventListener("click", closeValidation);
document.querySelector("#rules-form").addEventListener("submit", saveRules);
document.querySelector("#add-alias").addEventListener("click", () => {
  const row = appendAliasRow();
  row.querySelector(".alias-path").focus();
});
document.querySelector("#add-root").addEventListener("click", openNewRoot);
document.querySelector("#bootstrap-form").addEventListener("submit", bootstrapAdmin);
document.querySelector("#network-settings-form").addEventListener("submit", saveNetworkSettings);
document.querySelector("#indexing-settings-form").addEventListener("submit", saveIndexingSettings);
document.querySelector("#roots").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  const root = button.dataset.root;
  if (button.dataset.action === "rules") {
    openRules(root);
  } else if (button.dataset.action === "validate") {
    validateRoot(root);
  } else if (button.dataset.action === "repair") {
    repairRootIndex(root, { button });
  } else if (button.dataset.action === "crawl") {
    crawlRoot(root);
  } else if (button.dataset.action === "clear") {
    clearRootIndex(root);
  } else if (button.dataset.action === "delete") {
    deleteRoot(root);
  }
});

function headers() {
  const h = { "Content-Type": "application/json" };
  if (state.token) {
    h.Authorization = `Bearer ${state.token}`;
  }
  return h;
}

async function api(path, options = {}) {
  const res = await fetch(path, {
    ...options,
    cache: "no-store",
    headers: { ...headers(), ...(options.headers || {}) },
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : {};
  if (!res.ok) {
    const err = new Error(data.error?.message || res.statusText);
    err.code = data.error?.code;
    throw err;
  }
  return data;
}

async function publicAPI(path) {
  const res = await fetch(path, { cache: "no-store" });
  const text = await res.text();
  const data = text ? JSON.parse(text) : {};
  if (!res.ok) throw new Error(data.error?.message || res.statusText);
  return data;
}

async function refresh(options = {}) {
  let publicStatus;
  try {
    if (!options.quiet) setMessage("Refreshing...");
    publicStatus = await publicAPI("/admin/v1/status");
    renderService(publicStatus);
    if (!state.token) {
      showLockedStatus(publicStatus, options.message || "Enter an admin token to manage this service");
      return;
    }
    const [service, roots, crawls, config, metrics, logs] = await Promise.all([
      api("/admin/v1/service"),
      api("/admin/v1/roots"),
      api("/admin/v1/crawls"),
      api("/admin/v1/config"),
      api("/admin/v1/metrics"),
      api("/admin/v1/logs?limit=160").catch((err) => ({ error: err.message, lines: [] })),
    ]);
    renderService(service);
    renderMetrics(metrics);
    renderRoots(roots.roots || []);
    renderCrawls(crawls.crawls || []);
    renderRootErrors(roots.roots || []);
    renderLogs(logs);
    state.crawler = config.crawler || {};
    state.network = { server: config.server || {}, management: config.management || {} };
    renderNetworkSettings(state.network);
    renderIndexingSettings(state.crawler);
    document.querySelector("#config").textContent = JSON.stringify(config, null, 2);
    setAdminVisibility(true);
    setMessage(options.message || "Ready");
  } catch (err) {
    showLockedStatus(publicStatus, err.code === "admin_setup_required" ? "Set an admin token to continue" : "Admin token is missing or invalid");
  }
}

function startLiveMetrics() {
  if (liveMetricsTimer) return;
  liveMetricsTimer = setInterval(async () => {
    if (document.hidden || !state.token) return;
    try {
      renderMetrics(await api("/admin/v1/metrics"));
    } catch {
      // The next full refresh will present any authentication or service error.
    }
  }, 2000);

  liveDashboardTimer = setInterval(refreshLiveDashboard, 5000);
}

async function refreshLiveDashboard() {
  if (document.hidden || !state.token || liveDashboardRefreshing) return;
  liveDashboardRefreshing = true;
  try {
    const [service, roots, crawls, metrics, logs] = await Promise.all([
      api("/admin/v1/service"),
      api("/admin/v1/roots"),
      api("/admin/v1/crawls"),
      api("/admin/v1/metrics"),
      api("/admin/v1/logs?limit=160").catch((err) => ({ error: err.message, lines: [] })),
    ]);
    renderService(service);
    renderMetrics(metrics);
    renderRoots(roots.roots || []);
    renderCrawls(crawls.crawls || []);
    renderRootErrors(roots.roots || []);
    renderLogs(logs);
  } catch {
    // The normal refresh path presents authentication and availability errors.
  } finally {
    liveDashboardRefreshing = false;
  }
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden && state.token) refresh({ quiet: true });
});

async function bootstrapAdmin(event) {
  event.preventDefault();
  const token = document.querySelector("#bootstrap-token").value;
  const confirm = document.querySelector("#bootstrap-confirm").value;
  if (token !== confirm) {
    setMessage("Admin tokens do not match");
    return;
  }
  try {
    await api("/admin/v1/bootstrap", { method: "POST", body: JSON.stringify({ token }) });
    state.token = token;
    tokenInput.value = token;
    sessionStorage.setItem("qindexer.admin.token", token);
    document.querySelector("#bootstrap-token").value = "";
    document.querySelector("#bootstrap-confirm").value = "";
    document.querySelector("#bootstrap-panel").classList.add("hidden");
    refresh();
  } catch (err) {
    setMessage(err.message);
  }
}

function showBootstrap() {
  document.querySelector("#bootstrap-panel").classList.remove("hidden");
}

function showLockedStatus(status, detail) {
  resetPrivateDashboard();
  setAdminVisibility(false);
  const notice = document.querySelector("#auth-notice");
  notice.querySelector("span").textContent = status?.admin_configured === false
    ? "Set the first admin token locally to unlock administration. Root paths, rules, crawler activity, and logs remain hidden."
    : "Enter a valid admin token to view or manage indexed roots, paths, rules, crawler activity, and logs.";
  notice.classList.remove("hidden");
  if (status?.admin_configured === false) showBootstrap();
  else document.querySelector("#bootstrap-panel").classList.add("hidden");
  setMessage(detail);
}

function setAdminVisibility(authenticated) {
  document.querySelector("#admin-toolbar").classList.toggle("hidden", !authenticated);
  document.querySelector("#roots-view").classList.toggle("hidden", !authenticated);
  document.querySelector("#auth-notice").classList.toggle("hidden", authenticated);
  document.querySelectorAll(".admin-metric").forEach((element) => element.classList.toggle("hidden", !authenticated));
  if (authenticated) document.querySelector("#bootstrap-panel").classList.add("hidden");
}

function resetPrivateDashboard() {
  state.roots = [];
  state.crawler = {};
  state.network = {};
  document.querySelector("#roots").innerHTML = "";
  document.querySelector("#crawls").innerHTML = "";
  document.querySelector("#root-errors").innerHTML = "";
  document.querySelector("#logs").innerHTML = "";
  document.querySelector("#config").textContent = "{}";
  document.querySelector("#rules-panel").classList.add("hidden");
  document.querySelector("#validation-panel").classList.add("hidden");
}

function renderMetrics(metrics) {
  document.querySelector("#files-rate").textContent = formatRate(metrics.files_per_second || 0);
  document.querySelector("#dirs-rate").textContent = formatRate(metrics.directories_per_second || 0);
  document.querySelector("#io-rate").textContent = `${formatBytes(metrics.bytes_per_second || 0)}/s`;
  document.querySelector("#index-size").textContent = formatBytes(metrics.index_size_bytes || 0);
  const active = metrics.active_crawls || 0;
  document.querySelector("#content-queue").textContent = Number(metrics.content_queue_depth || 0).toLocaleString();
  document.querySelector("#ocr-queue").textContent = Number(metrics.ocr_queue_depth || 0).toLocaleString();
  document.querySelector("#hash-queue").textContent = Number(metrics.hash_queue_depth || 0).toLocaleString();
  document.querySelector("#next-crawl").textContent = formatFuture(metrics.next_full_crawl_unix);
  if (metrics.adaptive_paused) {
    document.querySelector("#crawler-state").textContent = `Throttled: ${String(metrics.pause_reason || "system").replace("adaptive_", "")}`;
    renderOps("Crawler throttled", `Paused by ${String(metrics.pause_reason || "system").replace("adaptive_", "")}. CPU ${formatRate(metrics.cpu_percent || 0)}%, disk busy ${formatRate(metrics.disk_busy_percent || 0)}%.`);
  } else if (metrics.paused) {
    document.querySelector("#crawler-state").textContent = `Paused ${formatPause(metrics.paused_until_unix)}`;
    renderOps("Crawler paused", `Manual or maintenance pause ends in ${formatPause(metrics.paused_until_unix) || "less than a second"}.`);
  } else {
    document.querySelector("#crawler-state").textContent = active ? `${active} active; background deferred` : "Idle";
    const roots = Array.isArray(metrics.active_roots) && metrics.active_roots.length ? ` ${metrics.active_roots.join(", ")}.` : "";
    const throughput = `${formatRate(metrics.files_per_second || 0)} files/s, ${formatRate(metrics.directories_per_second || 0)} dirs/s, ${formatBytes(metrics.bytes_per_second || 0)}/s.`;
    const activity = metrics.last_activity_unix ? ` Last observed crawl activity ${formatActivityAge(metrics.last_activity_unix)}.` : "";
    const deferred = ` Content, OCR, and hash backlogs wait for the structural crawl to finish; OCR candidates appear after content extraction identifies scanned or empty documents.`;
    renderOps(active ? "Crawler active" : "Crawler idle", active ? `${active} root crawl${active === 1 ? "" : "s"} running:${roots} ${throughput}.${activity}${deferred}` : `No active crawl. Background backlogs are eligible to drain. Next full reconciliation ${formatFuture(metrics.next_full_crawl_unix)}.`);
  }
}

function formatActivityAge(unix) {
  const seconds = Math.max(0, Math.round(Date.now() / 1000 - Number(unix)));
  if (seconds < 60) return "just now";
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m ago`;
}

function renderOps(title, detail) {
  document.querySelector("#ops-state").textContent = title;
  document.querySelector("#ops-detail").textContent = detail;
}

function renderService(service) {
  document.querySelector("#status").textContent = service.status || "unknown";
  document.querySelector("#version").textContent = service.version || "-";
  document.querySelector("#document-count").textContent = service.index?.document_count ?? 0;
}

function renderRoots(roots) {
  roots = roots.map(normalizeRoot);
  state.roots = roots;
  document.querySelector("#root-count").textContent = roots.length;
  const body = document.querySelector("#roots");
  body.innerHTML = "";
  if (!roots.length) {
    body.innerHTML = `<tr><td colspan="7">No configured roots.</td></tr>`;
    return;
  }
  for (const root of roots) {
    const tr = document.createElement("tr");
    const statusClass = statusTone(root.last_status);
    tr.innerHTML = `
      <td><strong>${escapeHtml(root.id)}</strong>${root.name ? `<span class="root-name">${escapeHtml(root.name)}</span>` : ""}<div>${labels(root.labels)}</div></td>
      <td class="root-path">${escapeHtml(root.path)}</td>
      <td><span class="tag ${statusClass}">${escapeHtml(root.last_status || "never")}</span></td>
      <td>${root.document_count ?? 0}</td>
      <td>${root.missing_count ?? 0}</td>
      <td class="rules">${ruleSummary(root)}</td>
      <td class="actions">
        <div class="action-group">
          <button type="button" class="secondary" data-action="rules" data-root="${escapeAttr(root.id)}">Rules</button>
          <button type="button" class="secondary" data-action="validate" data-root="${escapeAttr(root.id)}">Validate</button>
          <button type="button" class="secondary" data-action="repair" data-root="${escapeAttr(root.id)}">Repair index</button>
          <button type="button" data-action="crawl" data-root="${escapeAttr(root.id)}">Crawl</button>
          <button type="button" class="danger" data-action="clear" data-root="${escapeAttr(root.id)}">Clear index</button>
          <button type="button" class="danger" data-action="delete" data-root="${escapeAttr(root.id)}">Delete root</button>
        </div>
      </td>
    `;
    body.appendChild(tr);
  }
}

function openRules(rootId) {
  const root = state.roots.find((item) => item.id === rootId);
  if (!root) return;
  state.editingRoot = root;
  state.creatingRoot = false;
  document.querySelector("#rules-title").textContent = `Index Rules: ${root.id}`;
  document.querySelector("#save-repair-rules").classList.remove("hidden");
  document.querySelector("#rules-id").value = root.id;
  document.querySelector("#rules-id").disabled = true;
  document.querySelector("#rules-name").value = root.name || "";
  document.querySelector("#rules-path").value = root.path || "";
  document.querySelector("#rules-enabled").checked = Boolean(root.enabled);
  document.querySelector("#rules-labels").value = (root.labels || []).join(", ");
  document.querySelector("#rules-credential").value = root.credential_ref || "";
  setAliasRows(root.path_aliases);
  document.querySelector("#rules-include-ext").value = lines(root.include_extensions);
  document.querySelector("#rules-exclude-ext").value = lines(root.exclude_extensions);
  document.querySelector("#rules-include-file").value = lines(root.include_file_patterns);
  document.querySelector("#rules-exclude-file").value = lines(root.exclude_file_patterns);
  document.querySelector("#rules-include-folder").value = lines(root.include_folder_patterns);
  document.querySelector("#rules-exclude-folder").value = lines(root.exclude_folder_patterns);
  document.querySelector("#rules-exclude-legacy").value = lines(root.exclude_patterns);
  document.querySelector("#rules-extraction").checked = rootOption(root, "content_extraction", state.crawler.content_extraction?.enabled);
  document.querySelector("#rules-ocr").checked = rootOption(root, "ocr", state.crawler.ocr?.enabled);
  document.querySelector("#rules-hashing").checked = rootOption(root, "hashing", state.crawler.hashing?.enabled);
  document.querySelector("#rules-ownership").checked = rootOption(root, "collect_ownership", state.crawler.collect_ownership);
  document.querySelector("#rules-panel").classList.remove("hidden");
  document.querySelector("#rules-panel").scrollIntoView({ behavior: "smooth", block: "start" });
}

function openNewRoot() {
  state.editingRoot = null;
  state.creatingRoot = true;
  document.querySelector("#rules-title").textContent = "Add indexed root";
  document.querySelector("#save-repair-rules").classList.add("hidden");
  document.querySelector("#rules-id").value = "";
  document.querySelector("#rules-id").disabled = false;
  document.querySelector("#rules-name").value = "";
  document.querySelector("#rules-path").value = "";
  document.querySelector("#rules-enabled").checked = true;
  document.querySelector("#rules-labels").value = "";
  document.querySelector("#rules-credential").value = "";
  setAliasRows([]);
  document.querySelector("#rules-include-ext").value = "";
  document.querySelector("#rules-exclude-ext").value = "";
  document.querySelector("#rules-include-file").value = "";
  document.querySelector("#rules-exclude-file").value = "";
  document.querySelector("#rules-include-folder").value = "";
  document.querySelector("#rules-exclude-folder").value = "";
  document.querySelector("#rules-exclude-legacy").value = "";
  document.querySelector("#rules-extraction").checked = Boolean(state.crawler.content_extraction?.enabled);
  document.querySelector("#rules-ocr").checked = Boolean(state.crawler.ocr?.enabled);
  document.querySelector("#rules-hashing").checked = Boolean(state.crawler.hashing?.enabled);
  document.querySelector("#rules-ownership").checked = Boolean(state.crawler.collect_ownership);
  document.querySelector("#rules-panel").classList.remove("hidden");
  document.querySelector("#rules-panel").scrollIntoView({ behavior: "smooth", block: "start" });
}

function closeRules() {
  state.editingRoot = null;
  state.creatingRoot = false;
  document.querySelector("#rules-panel").classList.add("hidden");
}

async function saveRules(event) {
  event.preventDefault();
  if (!state.editingRoot && !state.creatingRoot) return;
  const repairAfterSave = event.submitter?.id === "save-repair-rules" && !state.creatingRoot;
  const rootId = state.creatingRoot ? document.querySelector("#rules-id").value.trim() : state.editingRoot.id;
  let aliases;
  try {
    aliases = readAliasRows();
  } catch (err) {
    setMessage(err.message);
    return;
  }
  const body = {
    id: rootId,
    name: document.querySelector("#rules-name").value.trim(),
    path: document.querySelector("#rules-path").value.trim(),
    enabled: document.querySelector("#rules-enabled").checked,
    labels: splitList(document.querySelector("#rules-labels").value),
    credential_ref: document.querySelector("#rules-credential").value.trim(),
    path_aliases: aliases,
    include_extensions: splitList(document.querySelector("#rules-include-ext").value),
    exclude_extensions: splitList(document.querySelector("#rules-exclude-ext").value),
    include_file_patterns: splitList(document.querySelector("#rules-include-file").value),
    exclude_file_patterns: splitList(document.querySelector("#rules-exclude-file").value),
    include_folder_patterns: splitList(document.querySelector("#rules-include-folder").value),
    exclude_folder_patterns: splitList(document.querySelector("#rules-exclude-folder").value),
    exclude_patterns: splitList(document.querySelector("#rules-exclude-legacy").value),
    content_extraction: document.querySelector("#rules-extraction").checked,
    ocr: document.querySelector("#rules-ocr").checked,
    hashing: document.querySelector("#rules-hashing").checked,
    collect_ownership: document.querySelector("#rules-ownership").checked,
  };
  try {
    const data = await api(state.creatingRoot ? "/admin/v1/roots" : `/admin/v1/roots/${encodeURIComponent(rootId)}/rules`, {
      method: state.creatingRoot ? "POST" : "PUT",
      body: JSON.stringify(body),
    });
    if (repairAfterSave) {
      const repair = await repairRootIndex(rootId, { confirm: false, refreshAfter: false });
      await refresh({ quiet: true, message: `${rootId}: saved, repaired ${repair.paths_rewritten || 0}, merged ${repair.duplicate_paths_merged || 0}` });
    } else {
      await refresh({ quiet: true, message: data.repair_required ? `${rootId}: saved; repair index when ready` : `${rootId}: ${data.status}` });
    }
    closeRules();
  } catch (err) {
    setMessage(err.message);
  }
}

function renderIndexingSettings(crawler) {
  const extraction = crawler.content_extraction || {};
  const ocr = crawler.ocr || {};
  const hashing = crawler.hashing || {};
  const throttle = crawler.adaptive_throttle || {};
  document.querySelector("#settings-extraction").checked = Boolean(extraction.enabled);
  document.querySelector("#settings-extraction-size").value = extraction.max_file_size_mb || 64;
  document.querySelector("#settings-ocr").checked = Boolean(ocr.enabled);
  document.querySelector("#settings-ocr-engine").value = ocr.engine || "auto";
  document.querySelector("#settings-ocr-languages").value = ocr.languages || "eng";
  document.querySelector("#settings-ocr-size").value = ocr.max_file_size_mb || 128;
  document.querySelector("#settings-ocr-timeout").value = ocr.timeout_seconds || 180;
  document.querySelector("#settings-tesseract-command").value = ocr.tesseract_command || "tesseract";
  document.querySelector("#settings-ocrmypdf-command").value = ocr.ocrmypdf_command || "ocrmypdf";
  document.querySelector("#settings-hashing").checked = Boolean(hashing.enabled);
  document.querySelector("#settings-hashing-size").value = hashing.max_file_size_mb || 2048;
  document.querySelector("#settings-ownership").checked = Boolean(crawler.collect_ownership);
  document.querySelector("#settings-throttle-enabled").checked = Boolean(throttle.enabled);
  document.querySelector("#settings-throttle-cpu").value = throttle.cpu_percent_threshold || 80;
  document.querySelector("#settings-throttle-disk").value = throttle.disk_busy_percent_threshold || 70;
  document.querySelector("#settings-throttle-sample").value = throttle.sample_interval_seconds || 5;
  document.querySelector("#settings-throttle-recovery").value = throttle.recovery_samples || 3;
}

function renderNetworkSettings(network) {
  const server = network.server || {};
  const management = network.management || {};
  document.querySelector("#settings-search-binds").value = lines(server.bind_addresses?.length ? server.bind_addresses : [server.bind || "127.0.0.1:41973"]);
  document.querySelector("#settings-admin-binds").value = lines(management.bind_addresses?.length ? management.bind_addresses : [management.bind || "127.0.0.1:41974"]);
  document.querySelector("#settings-public-url").value = server.public_base_url || "";
  document.querySelector("#settings-admin-enabled").checked = management.enabled !== false;
}

async function saveNetworkSettings(event) {
  event.preventDefault();
  const serverBinds = splitLines(document.querySelector("#settings-search-binds").value);
  const adminBinds = splitLines(document.querySelector("#settings-admin-binds").value);
  const body = {
    server: {
      bind_addresses: serverBinds,
      public_base_url: document.querySelector("#settings-public-url").value.trim(),
    },
    management: {
      enabled: document.querySelector("#settings-admin-enabled").checked,
      bind_addresses: adminBinds,
    },
  };
  try {
    const data = await api("/admin/v1/network", { method: "PUT", body: JSON.stringify(body) });
    await refresh({ quiet: true, message: data.restart_required ? "Network settings saved; restart required" : "Network settings saved" });
  } catch (err) {
    setMessage(err.message);
  }
}

async function saveIndexingSettings(event) {
  event.preventDefault();
  const crawler = state.crawler || {};
  const ocr = { ...(crawler.ocr || {}), enabled: document.querySelector("#settings-ocr").checked, engine: document.querySelector("#settings-ocr-engine").value, languages: document.querySelector("#settings-ocr-languages").value.trim() || "eng", max_file_size_mb: Number(document.querySelector("#settings-ocr-size").value) || 128, timeout_seconds: Number(document.querySelector("#settings-ocr-timeout").value) || 180, tesseract_command: document.querySelector("#settings-tesseract-command").value.trim() || "tesseract", ocrmypdf_command: document.querySelector("#settings-ocrmypdf-command").value.trim() || "ocrmypdf" };
  const content = { ...(crawler.content_extraction || {}), enabled: document.querySelector("#settings-extraction").checked || ocr.enabled, max_file_size_mb: Number(document.querySelector("#settings-extraction-size").value) || 64 };
  const hashing = { ...(crawler.hashing || {}), enabled: document.querySelector("#settings-hashing").checked, max_file_size_mb: Number(document.querySelector("#settings-hashing-size").value) || 2048 };
  const throttle = { ...(crawler.adaptive_throttle || {}), enabled: document.querySelector("#settings-throttle-enabled").checked, cpu_percent_threshold: Number(document.querySelector("#settings-throttle-cpu").value) || 80, disk_busy_percent_threshold: Number(document.querySelector("#settings-throttle-disk").value) || 70, sample_interval_seconds: Number(document.querySelector("#settings-throttle-sample").value) || 5, recovery_samples: Number(document.querySelector("#settings-throttle-recovery").value) || 3 };
  try {
    await api("/admin/v1/crawler/settings", { method: "PUT", body: JSON.stringify({ collect_ownership: document.querySelector("#settings-ownership").checked, adaptive_throttle: throttle, content_extraction: content, ocr, hashing }) });
    await refresh({ quiet: true, message: "Indexing settings saved" });
  } catch (err) { setMessage(err.message); }
}

function renderCrawls(crawls) {
  const list = document.querySelector("#crawls");
  list.innerHTML = "";
  if (!crawls.length) {
    list.innerHTML = `<div class="activity-empty"><strong>No crawls yet</strong><span>Trigger a root crawl to populate history.</span></div>`;
    return;
  }
  for (const crawl of crawls) {
    const item = document.createElement("div");
    item.className = "activity-item";
    const status = crawl.status || "unknown";
    item.innerHTML = `
      <div class="activity-main">
        <strong>${escapeHtml(crawl.root_id)}</strong>
        <span class="crawl-time">${escapeHtml(formatTimestamp(crawl.started_at || ""))}</span>
        ${crawl.error_message ? `<span class="crawl-error">${escapeHtml(crawl.error_message)}</span>` : ""}
      </div>
      <div class="activity-status"><span class="tag ${statusTone(status)}">${escapeHtml(status)}</span></div>
      <div class="activity-stats" aria-label="crawl totals">
        ${crawlStat("Seen", crawl.files_seen)}
        ${crawlStat("Added", crawl.files_added)}
        ${crawlStat("Updated", crawl.files_updated)}
        ${crawlStat("Missing", crawl.files_missing)}
        ${crawlStat("Errors", crawl.errors)}
      </div>
    `;
    list.appendChild(item);
  }
}

function renderRootErrors(roots) {
  const box = document.querySelector("#root-errors");
  const errors = roots
    .filter((root) => root.last_error || ["failed", "unreachable", "interrupted"].includes(root.last_status))
    .slice(0, 8);
  if (!errors.length) {
    box.innerHTML = `<div class="diagnostic-ok">No root-level errors reported.</div>`;
    return;
  }
  box.innerHTML = errors.map((root) => `
    <div class="root-error">
      <strong>${escapeHtml(root.id)}</strong>
      <span class="tag ${statusTone(root.last_status)}">${escapeHtml(root.last_status || "unknown")}</span>
      <p>${escapeHtml(root.last_error || "No error message recorded.")}</p>
    </div>
  `).join("");
}

async function refreshLogs() {
  try {
    const logs = await api("/admin/v1/logs?limit=240");
    renderLogs(logs);
    setMessage("Logs refreshed");
  } catch (err) {
    setMessage(err.message);
  }
}

function renderLogs(data) {
  const list = document.querySelector("#logs");
  if (data?.error) {
    list.innerHTML = `<div class="log-empty">Logs unavailable: ${escapeHtml(data.error)}</div>`;
    return;
  }
  const lines = array(data?.lines).slice(-160);
  if (!lines.length) {
    list.innerHTML = `<div class="log-empty">No log lines yet.</div>`;
    return;
  }
  list.innerHTML = lines.map((line) => {
    const lower = line.toLowerCase();
    const tone = lower.includes("level=error") || lower.includes(" panic ") || lower.includes("panic=") ? "error" : lower.includes("level=warn") ? "warn" : "info";
    return `<div class="log-line ${tone}">${escapeHtml(line)}</div>`;
  }).join("");
  list.scrollTop = list.scrollHeight;
}

function crawlStat(label, value) {
  return `<span><b>${Number(value || 0).toLocaleString()}</b>${label}</span>`;
}

async function validateConfig() {
  try {
    const data = await api("/admin/v1/config/validate", { method: "POST", body: "{}" });
    setMessage(`Config ${data.status}`);
  } catch (err) {
    setMessage(err.message);
  }
}

async function pauseCrawler() {
  const seconds = Number(document.querySelector("#pause-seconds").value) || 300;
  try {
    const data = await api("/admin/v1/crawler/pause", { method: "POST", body: JSON.stringify({ seconds }) });
    await refresh({ quiet: true, message: `Crawler paused until ${new Date((data.paused_until_unix || 0) * 1000).toLocaleTimeString()}` });
  } catch (err) {
    setMessage(err.message);
  }
}

async function resumeCrawler() {
  try {
    const data = await api("/admin/v1/crawler/resume", { method: "POST", body: "{}" });
    await refresh({ quiet: true, message: `Crawler ${data.status}` });
  } catch (err) {
    setMessage(err.message);
  }
}

async function stopCrawler() {
  try {
    const data = await api("/admin/v1/crawler/stop", { method: "POST", body: "{}" });
    const active = Array.isArray(data.active_roots) ? data.active_roots : [];
    const detail = active.length ? `; draining ${active.join(", ")}` : "";
    await refresh({ quiet: true, message: data.status === "stopped" ? "Crawls stopped" : `Crawls stopping${detail}` });
  } catch (err) {
    setMessage(err.message);
  }
}

async function stopService() {
  if (!window.confirm("Stop QIndexer now? Active crawls will checkpoint what has completed, database writes will flush, and the process will exit.")) return;
  try {
    const data = await api("/admin/v1/service/stop", { method: "POST", body: "{}" });
    setMessage(data.crawls_stopped ? "Stopping service..." : "Stopping service; waiting on slow filesystem calls");
  } catch (err) {
    setMessage(err.message);
  }
}

async function validateRoot(root) {
  try {
    const data = await api(`/admin/v1/roots/${encodeURIComponent(root)}/validate`, { method: "POST", body: "{}" });
    const count = data.expanded_count ?? 0;
    setMessage(`${root}: ${data.status}${data.error ? ` - ${data.error}` : `, ${count} expanded path${count === 1 ? "" : "s"}`}`);
    showValidation(root, data);
  } catch (err) {
    setMessage(err.message);
  }
}

async function crawlRoot(root) {
  try {
    const data = await api(`/admin/v1/roots/${encodeURIComponent(root)}/crawl`, { method: "POST", body: "{}" });
    await refresh({ quiet: true, message: `${root}: ${data.status}` });
    setTimeout(() => refresh({ quiet: true }), 1200);
  } catch (err) {
    setMessage(err.message);
  }
}

async function clearRootIndex(root) {
  if (!window.confirm(`Clear every indexed document, checkpoint, and crawl record for ${root}? This does not delete files.`)) return;
  try {
    const data = await api(`/admin/v1/roots/${encodeURIComponent(root)}/clear-index`, { method: "POST", body: JSON.stringify({ confirm_root_id: root }) });
    const count = data.documents_removed || 0;
    await refresh({ quiet: true, message: `${root}: cleared ${count} indexed document${count === 1 ? "" : "s"}` });
  } catch (err) {
    setMessage(err.message);
  }
}

async function repairRootIndex(root, options = {}) {
  const confirmRepair = options.confirm !== false;
  if (confirmRepair && !window.confirm(`Repair indexed paths for ${root}? This rewrites rows that match this root's aliases into the root's canonical path and merges duplicates. Source files are not changed.`)) return null;
  const button = options.button || null;
  const originalText = button?.textContent;
  if (button) {
    button.disabled = true;
    button.textContent = "Repairing...";
  }
  setMessage(`${root}: repairing index paths...`);
	const startedAt = Date.now();
	const showRepairProgress = async () => {
		try {
			const progress = await api("/admin/v1/operations/repair");
			if (progress.status !== "running" || progress.root_id !== root) return;
			const seconds = Math.max(0, Math.floor((Date.now() - startedAt) / 1000));
			const matched = progress.paths_matched || 0;
			const processed = progress.paths_processed || 0;
			const pathProgress = matched ? `; ${processed.toLocaleString()} / ${matched.toLocaleString()} matching paths processed` : "";
			const detail = `${progress.phase || "working"}; ${progress.aliases_checked || 0} aliases checked${pathProgress}, ${progress.paths_rewritten || 0} paths rewritten, ${progress.duplicate_paths_merged || 0} duplicates merged. ${seconds}s elapsed.`;
			renderOps(`Repairing ${root}`, detail);
			setMessage(`${root}: ${detail}`);
		} catch (_) {
			// The repair request itself reports failures; polling is supplementary.
		}
	};
	await showRepairProgress();
	const progressTimer = setInterval(showRepairProgress, 1000);
  try {
    const data = await api(`/admin/v1/roots/${encodeURIComponent(root)}/repair-index`, { method: "POST", body: "{}" });
    const rewritten = data.paths_rewritten || 0;
    const merged = data.duplicate_paths_merged || 0;
    const aliases = data.aliases_checked || 0;
    setMessage(`${root}: checked ${aliases} aliases, rewrote ${rewritten}, merged ${merged}`);
    showOperation("Index Repair", {
      root_id: data.root_id,
      status: data.status,
      aliases_checked: data.aliases_checked,
      aliases_repaired: data.aliases_repaired,
      paths_rewritten: rewritten,
      duplicate_paths_merged: merged,
      embedded_paths_rewritten: data.embedded_paths_rewritten || 0,
      embedded_paths_merged: data.embedded_paths_merged || 0,
    });
    if (options.refreshAfter !== false) {
      await refresh({ quiet: true, message: `${root}: checked ${aliases} aliases, rewrote ${rewritten}, merged ${merged}` });
    }
    return data;
  } catch (err) {
    setMessage(`${root}: repair failed - ${err.message}`);
    showOperation("Index Repair Failed", { root_id: root, error: err.message, code: err.code });
    throw err;
  } finally {
		clearInterval(progressTimer);
    if (button) {
      button.disabled = false;
      button.textContent = originalText;
    }
  }
}

async function deleteRoot(root) {
  if (!window.confirm(`Delete ${root} from QIndexer and remove all of its indexed documents, checkpoints, and crawl records? This does not delete files.`)) return;
  try {
    const data = await api(`/admin/v1/roots/${encodeURIComponent(root)}`, { method: "DELETE", body: JSON.stringify({ confirm_root_id: root }) });
    const count = data.documents_removed || 0;
    await refresh({ quiet: true, message: `${root}: removed ${count} indexed document${count === 1 ? "" : "s"}` });
  } catch (err) {
    setMessage(err.message);
  }
}

function labels(values = []) {
  values = array(values);
  return values.map((value) => `<span class="tag">${escapeHtml(value)}</span>`).join(" ");
}

function ruleSummary(root) {
  const includeTypes = array(root.include_extensions).length;
  const parts = [
    includeTypes ? `${includeTypes} type filter${includeTypes === 1 ? "" : "s"}` : "all file types",
    `${array(root.exclude_extensions).length} blocked type${array(root.exclude_extensions).length === 1 ? "" : "s"}`,
    `${array(root.include_file_patterns).length + array(root.exclude_file_patterns).length} file pattern${array(root.include_file_patterns).length + array(root.exclude_file_patterns).length === 1 ? "" : "s"}`,
    `${array(root.include_folder_patterns).length + array(root.exclude_folder_patterns).length} folder pattern${array(root.include_folder_patterns).length + array(root.exclude_folder_patterns).length === 1 ? "" : "s"}`,
    rootOption(root, "content_extraction", state.crawler.content_extraction?.enabled) ? "content" : "no content",
    rootOption(root, "ocr", state.crawler.ocr?.enabled) ? "OCR" : "no OCR",
    rootOption(root, "hashing", state.crawler.hashing?.enabled) ? "SHA-256" : "no hashes",
  ];
  return `<div class="root-rule-summary">${parts.map((part) => `<span class="tag">${escapeHtml(part)}</span>`).join("")}</div>`;
}

function splitList(value) {
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function splitLines(value) {
  return value
    .split("\n")
    .map((item) => item.trim())
    .filter(Boolean);
}

function lines(values = []) {
  return array(values).join("\n");
}

function setAliasRows(values = []) {
  const container = document.querySelector("#rules-aliases");
  container.replaceChildren();
  array(values).forEach((alias) => appendAliasRow(alias));
  renderAliasEmptyState();
}

function appendAliasRow(alias = {}) {
  const container = document.querySelector("#rules-aliases");
  const row = document.createElement("div");
  row.className = "alias-row";

  const label = document.createElement("input");
  label.className = "alias-label";
  label.type = "text";
  label.placeholder = "Optional label";
  label.value = alias.id || "";
  label.setAttribute("aria-label", "Alias label");

  const platform = document.createElement("select");
  platform.className = "alias-platform";
  platform.setAttribute("aria-label", "Client platform");
  const platforms = [
    ["windows-drive", "Windows drive"],
    ["windows-unc", "Windows UNC"],
    ["linux", "Linux / macOS"],
    ["service", "Service path"],
  ];
  const selectedPlatform = alias.platform || "windows-drive";
  if (!platforms.some(([value]) => value === selectedPlatform)) platforms.push([selectedPlatform, selectedPlatform]);
  platforms.forEach(([value, text]) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = text;
    option.selected = value === selectedPlatform;
    platform.appendChild(option);
  });

  const path = document.createElement("input");
  path.className = "alias-path";
  path.type = "text";
  path.placeholder = "X:\\Shared or /mnt/shared";
  path.value = alias.path || "";
  path.setAttribute("aria-label", "Client path");

  const target = document.createElement("input");
  target.className = "alias-target";
  target.type = "text";
  target.placeholder = "Optional indexed target";
  target.value = alias.target || "";
  target.setAttribute("aria-label", "Indexed target override");

  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "secondary";
  remove.textContent = "\u00d7";
  remove.title = "Remove alias";
  remove.setAttribute("aria-label", "Remove alias");
  remove.addEventListener("click", () => {
    row.remove();
    renderAliasEmptyState();
  });

  row.append(label, platform, path, target, remove);
  container.appendChild(row);
  renderAliasEmptyState();
  return row;
}

function renderAliasEmptyState() {
  const container = document.querySelector("#rules-aliases");
  container.querySelector(".alias-empty")?.remove();
  if (container.querySelector(".alias-row")) return;
  const empty = document.createElement("div");
  empty.className = "alias-empty";
  empty.textContent = "No client aliases. Add one only when clients reach this root through a different path.";
  container.appendChild(empty);
}

function readAliasRows() {
  const rows = Array.from(document.querySelectorAll("#rules-aliases .alias-row"));
  const aliases = [];
  for (const row of rows) {
    const id = row.querySelector(".alias-label").value.trim();
    const platform = row.querySelector(".alias-platform").value.trim();
    const path = row.querySelector(".alias-path").value.trim();
    const target = row.querySelector(".alias-target").value.trim();
    if (!id && !path && !target) continue;
    if (!path) throw new Error("Every client path alias needs a client path or should be removed.");
    aliases.push({ id, platform, path, target });
  }
  return aliases;
}

function array(values) {
  return Array.isArray(values) ? values : [];
}

function normalizeRoot(root) {
  return {
    ...root,
    labels: array(root.labels),
    include_extensions: array(root.include_extensions),
    exclude_extensions: array(root.exclude_extensions),
    include_file_patterns: array(root.include_file_patterns),
    exclude_file_patterns: array(root.exclude_file_patterns),
    include_folder_patterns: array(root.include_folder_patterns),
    exclude_folder_patterns: array(root.exclude_folder_patterns),
    exclude_patterns: array(root.exclude_patterns),
    path_aliases: array(root.path_aliases),
  };
}

function rootOption(root, field, fallback) {
  return typeof root[field] === "boolean" ? root[field] : Boolean(fallback);
}

function showView(view) {
  const configView = document.querySelector("#config-view");
  const rootsButton = document.querySelector("#view-roots");
  const configButton = document.querySelector("#view-config");
  if (view === "config") {
    configView.classList.remove("hidden");
    configView.scrollIntoView({ behavior: "smooth", block: "start" });
    rootsButton.classList.add("secondary");
    configButton.classList.remove("secondary");
    history.replaceState(null, "", "/config");
    return;
  }
  configView.classList.add("hidden");
  rootsButton.classList.remove("secondary");
  configButton.classList.add("secondary");
  history.replaceState(null, "", "/roots");
}

function showValidation(root, data) {
  document.querySelector("#validation-title").textContent = `Path Validation: ${root}`;
  const paths = data.expanded_paths || [];
  const shown = paths.slice(0, 100);
  document.querySelector("#validation-output").textContent = JSON.stringify({
    status: data.status,
    expanded_count: data.expanded_count,
    expanded_paths_sample: shown,
    omitted_paths: Math.max(0, paths.length - shown.length),
    error: data.error,
  }, null, 2);
  document.querySelector("#validation-panel").classList.remove("hidden");
}

function showOperation(title, data) {
  document.querySelector("#validation-title").textContent = title;
  document.querySelector("#validation-output").textContent = JSON.stringify(data, null, 2);
  document.querySelector("#validation-panel").classList.remove("hidden");
}

function closeValidation() {
  document.querySelector("#validation-panel").classList.add("hidden");
}

function statusTone(status = "") {
  if (["ok", "reachable", "hint_ok"].includes(status)) return "ok";
  if (["never", "running", "scheduled", "already_running", "hint_running"].includes(status)) return "warn";
  return "bad";
}

function setMessage(text) {
  message.textContent = text;
}

function formatRate(value) {
  if (value >= 100) return Math.round(value).toLocaleString();
  if (value >= 10) return value.toFixed(1);
  return value.toFixed(2);
}

function formatBytes(value) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = Number(value) || 0;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit++;
  }
  return `${size >= 10 || unit === 0 ? size.toFixed(0) : size.toFixed(1)} ${units[unit]}`;
}

function formatPause(unix) {
  if (!unix) return "";
  const seconds = Math.max(0, Math.round(unix - Date.now() / 1000));
  if (seconds >= 3600) return `${Math.ceil(seconds / 3600)}h`;
  if (seconds >= 60) return `${Math.ceil(seconds / 60)}m`;
  return `${seconds}s`;
}

function formatFuture(unix) {
  if (!unix) return "-";
  const seconds = Math.round(unix - Date.now() / 1000);
  if (seconds <= 0) return "now";
  if (seconds >= 86400) return `${Math.ceil(seconds / 86400)}d`;
  if (seconds >= 3600) return `${Math.ceil(seconds / 3600)}h`;
  if (seconds >= 60) return `${Math.ceil(seconds / 60)}m`;
  return `${seconds}s`;
}

function formatTimestamp(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString([], { dateStyle: "medium", timeStyle: "medium" });
}

function escapeHtml(value) {
  return String(value ?? "").replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[ch]));
}

function escapeAttr(value) {
  return escapeHtml(value).replace(/`/g, "&#96;");
}

refresh().then(() => {
  startLiveMetrics();
  if (location.pathname === "/config" || location.hash === "#config") {
    showView("config");
  }
});
