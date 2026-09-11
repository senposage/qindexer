const tokenInput = document.querySelector("#token");
const message = document.querySelector("#message");
const endpoint = document.querySelector("#endpoint");

const state = {
  token: sessionStorage.getItem("qindexer.admin.token") || "",
  roots: [],
  editingRoot: null,
  creatingRoot: false,
  crawler: {},
};

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
  setMessage("Token cleared");
});

document.querySelector("#refresh").addEventListener("click", refresh);
document.querySelector("#validate-config").addEventListener("click", validateConfig);
document.querySelector("#pause-crawler").addEventListener("click", pauseCrawler);
document.querySelector("#resume-crawler").addEventListener("click", resumeCrawler);
document.querySelector("#view-roots").addEventListener("click", () => showView("roots"));
document.querySelector("#view-config").addEventListener("click", () => showView("config"));
document.querySelector("#close-rules").addEventListener("click", closeRules);
document.querySelector("#cancel-rules").addEventListener("click", closeRules);
document.querySelector("#close-validation").addEventListener("click", closeValidation);
document.querySelector("#rules-form").addEventListener("submit", saveRules);
document.querySelector("#add-root").addEventListener("click", openNewRoot);
document.querySelector("#bootstrap-form").addEventListener("submit", bootstrapAdmin);
document.querySelector("#indexing-settings-form").addEventListener("submit", saveIndexingSettings);
document.querySelector("#roots").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  const root = button.dataset.root;
  if (button.dataset.action === "rules") {
    openRules(root);
  } else if (button.dataset.action === "validate") {
    validateRoot(root);
  } else if (button.dataset.action === "crawl") {
    crawlRoot(root);
  } else if (button.dataset.action === "clear") {
    clearRootIndex(root);
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

async function refresh() {
  try {
    setMessage("Refreshing...");
    const [service, roots, crawls, config, metrics] = await Promise.all([
      api("/admin/v1/service"),
      api("/admin/v1/roots"),
      api("/admin/v1/crawls"),
      api("/admin/v1/config"),
      api("/admin/v1/metrics"),
    ]);
    renderService(service);
    renderMetrics(metrics);
    renderRoots(roots.roots || []);
    renderCrawls(crawls.crawls || []);
    state.crawler = config.crawler || {};
    renderIndexingSettings(state.crawler);
    document.querySelector("#config").textContent = JSON.stringify(config, null, 2);
    setMessage("Ready");
  } catch (err) {
    if (err.code === "admin_setup_required") {
      showBootstrap();
      setMessage("Set an admin token to continue");
      return;
    }
    setMessage(err.message);
  }
}

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

function renderMetrics(metrics) {
  document.querySelector("#files-rate").textContent = formatRate(metrics.files_per_second || 0);
  document.querySelector("#dirs-rate").textContent = formatRate(metrics.directories_per_second || 0);
  document.querySelector("#io-rate").textContent = `${formatBytes(metrics.bytes_per_second || 0)}/s`;
  const active = metrics.active_crawls || 0;
  if (metrics.adaptive_paused) {
    document.querySelector("#crawler-state").textContent = `Throttled: ${String(metrics.pause_reason || "system").replace("adaptive_", "")}`;
  } else if (metrics.paused) {
    document.querySelector("#crawler-state").textContent = `Paused ${formatPause(metrics.paused_until_unix)}`;
  } else {
    document.querySelector("#crawler-state").textContent = active ? `${active} active` : "Idle";
  }
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
          <button type="button" data-action="crawl" data-root="${escapeAttr(root.id)}">Crawl</button>
          <button type="button" class="danger" data-action="clear" data-root="${escapeAttr(root.id)}">Clear index</button>
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
  document.querySelector("#rules-id").value = root.id;
  document.querySelector("#rules-id").disabled = true;
  document.querySelector("#rules-name").value = root.name || "";
  document.querySelector("#rules-path").value = root.path || "";
  document.querySelector("#rules-enabled").checked = Boolean(root.enabled);
  document.querySelector("#rules-labels").value = (root.labels || []).join(", ");
  document.querySelector("#rules-credential").value = root.credential_ref || "";
  document.querySelector("#rules-aliases").value = aliasLines(root.path_aliases);
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
  document.querySelector("#rules-id").value = "";
  document.querySelector("#rules-id").disabled = false;
  document.querySelector("#rules-name").value = "";
  document.querySelector("#rules-path").value = "";
  document.querySelector("#rules-enabled").checked = true;
  document.querySelector("#rules-labels").value = "";
  document.querySelector("#rules-credential").value = "";
  document.querySelector("#rules-aliases").value = "";
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
  const rootId = state.creatingRoot ? document.querySelector("#rules-id").value.trim() : state.editingRoot.id;
  const body = {
    id: rootId,
    name: document.querySelector("#rules-name").value.trim(),
    path: document.querySelector("#rules-path").value.trim(),
    enabled: document.querySelector("#rules-enabled").checked,
    labels: splitList(document.querySelector("#rules-labels").value),
    credential_ref: document.querySelector("#rules-credential").value.trim(),
    path_aliases: parseAliases(document.querySelector("#rules-aliases").value),
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
    setMessage(`${rootId}: ${data.status}`);
    closeRules();
    refresh();
  } catch (err) {
    setMessage(err.message);
  }
}

function renderIndexingSettings(crawler) {
  const extraction = crawler.content_extraction || {};
	const ocr = crawler.ocr || {};
  const hashing = crawler.hashing || {};
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
}

async function saveIndexingSettings(event) {
  event.preventDefault();
  const crawler = state.crawler || {};
  const ocr = { ...(crawler.ocr || {}), enabled: document.querySelector("#settings-ocr").checked, engine: document.querySelector("#settings-ocr-engine").value, languages: document.querySelector("#settings-ocr-languages").value.trim() || "eng", max_file_size_mb: Number(document.querySelector("#settings-ocr-size").value) || 128, timeout_seconds: Number(document.querySelector("#settings-ocr-timeout").value) || 180, tesseract_command: document.querySelector("#settings-tesseract-command").value.trim() || "tesseract", ocrmypdf_command: document.querySelector("#settings-ocrmypdf-command").value.trim() || "ocrmypdf" };
  const content = { ...(crawler.content_extraction || {}), enabled: document.querySelector("#settings-extraction").checked || ocr.enabled, max_file_size_mb: Number(document.querySelector("#settings-extraction-size").value) || 64 };
  const hashing = { ...(crawler.hashing || {}), enabled: document.querySelector("#settings-hashing").checked, max_file_size_mb: Number(document.querySelector("#settings-hashing-size").value) || 2048 };
  try {
    await api("/admin/v1/crawler/settings", { method: "PUT", body: JSON.stringify({ collect_ownership: document.querySelector("#settings-ownership").checked, content_extraction: content, ocr, hashing }) });
    setMessage("Indexing settings saved");
    refresh();
  } catch (err) { setMessage(err.message); }
}

function renderCrawls(crawls) {
  const list = document.querySelector("#crawls");
  list.innerHTML = "";
  if (!crawls.length) {
    list.innerHTML = `<div class="list-item"><strong>No crawls yet</strong><span>Trigger a root crawl to populate history.</span></div>`;
    return;
  }
  for (const crawl of crawls) {
    const item = document.createElement("div");
    item.className = "list-item";
    item.innerHTML = `
      <strong>${escapeHtml(crawl.root_id)} <span class="tag ${statusTone(crawl.status)}">${escapeHtml(crawl.status)}</span></strong>
      <span class="crawl-meta">Seen ${crawl.files_seen} &middot; Added ${crawl.files_added} &middot; Updated ${crawl.files_updated} &middot; Missing ${crawl.files_missing} &middot; Errors ${crawl.errors}</span>
      <span class="crawl-time">${escapeHtml(formatTimestamp(crawl.started_at || ""))}</span>
      ${crawl.error_message ? `<span>${escapeHtml(crawl.error_message)}</span>` : ""}
    `;
    list.appendChild(item);
  }
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
    setMessage(`Crawler paused until ${new Date((data.paused_until_unix || 0) * 1000).toLocaleTimeString()}`);
    refresh();
  } catch (err) {
    setMessage(err.message);
  }
}

async function resumeCrawler() {
  try {
    const data = await api("/admin/v1/crawler/resume", { method: "POST", body: "{}" });
    setMessage(`Crawler ${data.status}`);
    refresh();
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
    setMessage(`${root}: ${data.status}`);
    setTimeout(refresh, 1200);
  } catch (err) {
    setMessage(err.message);
  }
}

async function clearRootIndex(root) {
  if (!window.confirm(`Clear every indexed document, checkpoint, and crawl record for ${root}? This does not delete files.`)) return;
  try {
    const data = await api(`/admin/v1/roots/${encodeURIComponent(root)}/clear-index`, { method: "POST", body: JSON.stringify({ confirm_root_id: root }) });
    const count = data.documents_removed || 0;
    setMessage(`${root}: cleared ${count} indexed document${count === 1 ? "" : "s"}`);
    refresh();
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

function lines(values = []) {
  return array(values).join("\n");
}

function aliasLines(values = []) {
  return array(values).map((alias) => [alias.id || "", alias.platform || "", alias.path || ""].join(" | ")).join("\n");
}

function parseAliases(value) {
  return value.split("\n").map((line) => line.split("|").map((part) => part.trim())).filter((parts) => parts.length === 3 && parts[1] && parts[2]).map(([id, platform, path]) => ({ id, platform, path }));
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
  if (location.pathname === "/config" || location.hash === "#config") {
    showView("config");
  }
});
