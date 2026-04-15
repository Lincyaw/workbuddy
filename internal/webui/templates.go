package webui

// listHTML is the template for the session list page.
const listHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sessions - workbuddy</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f6f8fa; color: #24292f; line-height: 1.5; }
  .container { max-width: 960px; margin: 0 auto; padding: 24px 16px; }
  h1 { margin-bottom: 16px; font-size: 24px; }
  .filters { display: flex; gap: 8px; margin-bottom: 16px; flex-wrap: wrap; }
  .filters input { padding: 6px 12px; border: 1px solid #d0d7de; border-radius: 6px; font-size: 14px; }
  .filters button { padding: 6px 16px; border: 1px solid #d0d7de; border-radius: 6px; background: #f6f8fa; cursor: pointer; font-size: 14px; }
  .filters button:hover { background: #eaeef2; }
  table { width: 100%; border-collapse: collapse; background: #fff; border: 1px solid #d0d7de; border-radius: 6px; overflow: hidden; }
  th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid #d0d7de; font-size: 14px; }
  th { background: #f6f8fa; font-weight: 600; }
  tr:last-child td { border-bottom: none; }
  tr:hover td { background: #f6f8fa; }
  a { color: #0969da; text-decoration: none; }
  a:hover { text-decoration: underline; }
  code { background: #f0f3f6; padding: 2px 6px; border-radius: 4px; font-size: 13px; }
  .empty { text-align: center; padding: 32px; color: #656d76; }
</style>
</head>
<body>
<div class="container">
  <h1>Agent Sessions</h1>

  <form class="filters" method="get" action="/sessions">
    <input type="text" name="repo" placeholder="Repo" value="{{.Filter.Repo}}">
    <input type="text" name="issue" placeholder="Issue #" value="{{if .Filter.IssueNum}}{{.Filter.IssueNum}}{{end}}">
    <input type="text" name="agent" placeholder="Agent name" value="{{.Filter.AgentName}}">
    <button type="submit">Filter</button>
  </form>

  {{if .Sessions}}
  <table>
    <thead>
      <tr>
        <th>Session ID</th>
        <th>Agent</th>
        <th>Repo</th>
        <th>Issue</th>
        <th>Created</th>
      </tr>
    </thead>
    <tbody>
    {{range .Sessions}}
      <tr>
        <td><a href="/sessions/{{.SessionID}}"><code>{{truncate .SessionID 16}}</code></a></td>
        <td>{{.AgentName}}</td>
        <td>{{.Repo}}</td>
        <td>#{{.IssueNum}}</td>
        <td>{{.CreatedAt.Format "2006-01-02 15:04:05"}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <div class="empty">No sessions found.</div>
  {{end}}
</div>
</body>
</html>`

// detailHTML is the template for the session detail page.
// Renders a live-updating timeline of Event Schema v1 events with color-coded
// kinds, collapsible payloads, tail-follow toggle, and SSE-backed streaming.
const detailHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Session {{.Session.SessionID}} - workbuddy</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f6f8fa; color: #24292f; line-height: 1.5; }
  .container { max-width: 1080px; margin: 0 auto; padding: 24px 16px 96px; }
  h1 { margin-bottom: 8px; font-size: 22px; }
  h2 { font-size: 16px; margin-bottom: 10px; }
  .breadcrumb { margin-bottom: 16px; font-size: 13px; color: #656d76; }
  .breadcrumb a { color: #0969da; text-decoration: none; }
  .breadcrumb a:hover { text-decoration: underline; }
  a { color: #0969da; text-decoration: none; }
  a:hover { text-decoration: underline; }
  code, .mono { font-family: "SF Mono", ui-monospace, Menlo, Consolas, monospace; font-size: 12.5px; }
  code { background: #eef1f4; padding: 1px 6px; border-radius: 4px; }
  .card { background: #fff; border: 1px solid #d0d7de; border-radius: 8px; padding: 14px 16px; margin-bottom: 14px; }
  .meta-grid { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; font-size: 13px; }
  .meta-grid dt { color: #656d76; font-weight: 600; }
  .meta-grid dd { overflow-wrap: anywhere; }
  .toolbar { position: sticky; top: 0; z-index: 5; display: flex; align-items: center; gap: 8px; padding: 10px 12px; margin-bottom: 14px; background: rgba(246,248,250,0.92); backdrop-filter: blur(6px); border: 1px solid #d0d7de; border-radius: 8px; flex-wrap: wrap; }
  .toolbar .grow { flex: 1 1 auto; }
  .toolbar select, .toolbar input, .toolbar button { font-size: 13px; padding: 4px 10px; border: 1px solid #d0d7de; border-radius: 6px; background: #fff; color: #24292f; }
  .toolbar button { cursor: pointer; }
  .toolbar button:hover { background: #f3f5f7; }
  .toolbar button.active { background: #dbeeff; border-color: #7abaf5; color: #0550a0; }
  .status { font-size: 12px; color: #656d76; }
  .status.live::before { content: ""; display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #2da44e; margin-right: 6px; vertical-align: middle; animation: pulse 1.6s infinite; }
  @keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: 0.35; } }
  .timeline { display: flex; flex-direction: column; gap: 8px; }
  .event { background: #fff; border: 1px solid #d0d7de; border-left-width: 3px; border-radius: 6px; padding: 8px 12px; font-size: 13px; }
  .event.hidden { display: none; }
  .event-header { display: flex; align-items: baseline; gap: 10px; cursor: pointer; user-select: none; }
  .kind-badge { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 11px; font-weight: 600; letter-spacing: 0.2px; }
  .event-title { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1 1 auto; }
  .event-time { color: #8c959f; font-size: 11px; flex: 0 0 auto; }
  .event-body { margin-top: 8px; border-top: 1px dashed #e2e5e8; padding-top: 8px; }
  .event-body pre { background: #f6f8fa; border-radius: 4px; padding: 8px 10px; overflow: auto; max-height: 360px; font-size: 12px; white-space: pre-wrap; word-break: break-word; }
  .event.collapsed .event-body { display: none; }
  /* kind palette */
  .k-agent_message  { border-left-color: #6f42c1; }
  .k-agent_message  .kind-badge { background: #efe2fa; color: #5a32a3; }
  .k-reasoning      { border-left-color: #8b949e; }
  .k-reasoning      .kind-badge { background: #eef1f4; color: #57606a; }
  .k-tool_call      { border-left-color: #0969da; }
  .k-tool_call      .kind-badge { background: #ddf4ff; color: #0550a0; }
  .k-tool_result    { border-left-color: #1f883d; }
  .k-tool_result    .kind-badge { background: #dcffe4; color: #116329; }
  .k-command_exec   { border-left-color: #bf8700; }
  .k-command_exec   .kind-badge { background: #fff1cc; color: #7d4e00; }
  .k-command_output { border-left-color: #d4a72c; }
  .k-command_output .kind-badge { background: #fff8c5; color: #7d4e00; }
  .k-file_change    { border-left-color: #0969da; }
  .k-file_change    .kind-badge { background: #ddf4ff; color: #0550a0; }
  .k-turn_started   { border-left-color: #2da44e; }
  .k-turn_started   .kind-badge { background: #dcffe4; color: #116329; }
  .k-turn_completed { border-left-color: #2da44e; }
  .k-turn_completed .kind-badge { background: #dcffe4; color: #116329; }
  .k-token_usage    { border-left-color: #8250df; }
  .k-token_usage    .kind-badge { background: #efe2fa; color: #5a32a3; }
  .k-log            { border-left-color: #8c959f; }
  .k-log            .kind-badge { background: #eef1f4; color: #57606a; }
  .k-error          { border-left-color: #cf222e; background: #fff5f5; }
  .k-error          .kind-badge { background: #ffe2e5; color: #a40e26; }
  .empty { text-align: center; padding: 32px; color: #656d76; font-size: 14px; }
  details.summary-fallback > summary { cursor: pointer; font-weight: 600; margin-bottom: 8px; font-size: 14px; }
  details.summary-fallback pre { background: #f6f8fa; border: 1px solid #d0d7de; border-radius: 6px; padding: 12px; overflow-x: auto; font-size: 12.5px; white-space: pre-wrap; word-wrap: break-word; }
</style>
</head>
<body>
<div class="container">
  <div class="breadcrumb"><a href="/sessions">Sessions</a> / <code>{{.Session.SessionID}}</code></div>
  <h1>Session · {{.Session.AgentName}} · {{.Session.Repo}}#{{.Session.IssueNum}}</h1>

  <div class="card">
    <dl class="meta-grid">
      <dt>Session ID</dt><dd><code>{{.Session.SessionID}}</code></dd>
      <dt>Task ID</dt><dd><code>{{.Session.TaskID}}</code></dd>
      <dt>Agent</dt><dd>{{.Session.AgentName}}</dd>
      <dt>Repo</dt><dd>{{.Session.Repo}}</dd>
      <dt>Issue</dt><dd>#{{.Session.IssueNum}}</dd>
      {{if .TaskStatus}}<dt>Task Status</dt><dd>{{.TaskStatus}}</dd>{{end}}
      <dt>Created</dt><dd>{{.Session.CreatedAt.Format "2006-01-02 15:04:05 UTC"}}</dd>
      {{if .Session.RawPath}}<dt>Raw File</dt><dd><code>{{.Session.RawPath}}</code></dd>{{end}}
    </dl>
  </div>

  {{if .HasEvents}}
  <div class="toolbar" id="toolbar">
    <strong style="font-size:14px;">Event Timeline</strong>
    <select id="filter-kind">
      <option value="">All kinds</option>
      <option value="agent.message">agent.message</option>
      <option value="reasoning">reasoning</option>
      <option value="tool.call">tool.call</option>
      <option value="tool.result">tool.result</option>
      <option value="command.exec">command.exec</option>
      <option value="command.output">command.output</option>
      <option value="file.change">file.change</option>
      <option value="turn.started">turn.started</option>
      <option value="turn.completed">turn.completed</option>
      <option value="token.usage">token.usage</option>
      <option value="error">error</option>
      <option value="log">log</option>
    </select>
    <input id="filter-text" type="search" placeholder="Search payload…" style="min-width:180px;">
    <button id="toggle-follow" class="active" type="button">Follow tail</button>
    <button id="expand-all" type="button">Expand all</button>
    <button id="collapse-all" type="button">Collapse all</button>
    <span class="grow"></span>
    <span class="status" id="status">connecting…</span>
  </div>
  <div class="timeline" id="timeline"></div>
  <div class="empty" id="empty" style="display:none;">No events yet.</div>
  {{end}}

  {{if .Session.Summary}}
  <div class="card">
    <details class="summary-fallback"{{if not .HasEvents}} open{{end}}>
      <summary>Textual summary (legacy)</summary>
      <pre>{{.Session.Summary}}</pre>
    </details>
  </div>
  {{end}}
</div>

{{if .HasEvents}}
<script>
(function() {
  const SID = {{printf "%q" .Session.SessionID}};
  const timeline = document.getElementById("timeline");
  const statusEl = document.getElementById("status");
  const emptyEl = document.getElementById("empty");
  const kindSel = document.getElementById("filter-kind");
  const textIn = document.getElementById("filter-text");
  const followBtn = document.getElementById("toggle-follow");
  let follow = true;
  let seen = new Set();

  function setStatus(msg, live) {
    statusEl.textContent = msg;
    statusEl.className = "status" + (live ? " live" : "");
  }

  function kindClass(k) { return "k-" + String(k || "unknown").replace(/\./g, "_"); }

  function eventTitle(ev) {
    const p = ev.payload || {};
    switch (ev.kind) {
      case "agent.message":   return shorten(p.text || p.content || "");
      case "reasoning":       return shorten(p.text || p.summary || "");
      case "tool.call":       return (p.name || p.tool || "tool") + "(" + Object.keys(p.input || p.arguments || {}).join(", ") + ")";
      case "tool.result":     return (p.name || p.tool || "result") + (p.is_error ? " · ERROR" : "");
      case "command.exec":    return shorten(Array.isArray(p.command) ? p.command.join(" ") : (p.command || ""));
      case "command.output":  return shorten(p.chunk || p.output || p.text || "");
      case "file.change":     return (p.action || "change") + " " + (p.path || "");
      case "turn.started":    return "turn " + (p.turn_id || ev.turn_id || "");
      case "turn.completed":  return "turn " + (p.turn_id || ev.turn_id || "") + (p.status ? " · " + p.status : "");
      case "token.usage":     return (p.input_tokens || 0) + " in / " + (p.output_tokens || 0) + " out";
      case "error":           return shorten(p.message || p.error || "error");
      case "log":             return shorten(p.message || p.text || "");
    }
    return "";
  }

  function shorten(s) {
    s = String(s || "").replace(/\s+/g, " ").trim();
    return s.length > 140 ? s.slice(0, 140) + "…" : s;
  }

  function renderEvent(ev) {
    if (seen.has(ev.index)) return;
    seen.add(ev.index);
    const el = document.createElement("div");
    el.className = "event collapsed " + kindClass(ev.kind);
    el.dataset.kind = ev.kind || "";
    const payloadText = ev.payload == null ? "" : JSON.stringify(ev.payload, null, 2);
    el.dataset.search = (ev.kind + " " + payloadText).toLowerCase();

    const hdr = document.createElement("div");
    hdr.className = "event-header";
    const badge = document.createElement("span");
    badge.className = "kind-badge";
    badge.textContent = ev.kind || "?";
    const title = document.createElement("span");
    title.className = "event-title";
    title.textContent = eventTitle(ev);
    const time = document.createElement("span");
    time.className = "event-time";
    time.textContent = ev.ts ? new Date(ev.ts).toLocaleTimeString() : ("#" + ev.index);
    hdr.append(badge, title, time);

    const body = document.createElement("div");
    body.className = "event-body";
    const pre = document.createElement("pre");
    pre.textContent = payloadText || "(no payload)";
    body.append(pre);
    if (ev.truncated) {
      const note = document.createElement("div");
      note.style.fontSize = "11px"; note.style.color = "#8c959f"; note.style.marginTop = "6px";
      note.textContent = "payload truncated for transport";
      body.append(note);
    }

    hdr.addEventListener("click", () => el.classList.toggle("collapsed"));
    el.append(hdr, body);
    applyFilter(el);
    timeline.append(el);
    if (follow) el.scrollIntoView({ block: "end", behavior: "instant" });
  }

  function applyFilter(el) {
    const k = kindSel.value;
    const q = textIn.value.trim().toLowerCase();
    const kindOK = !k || el.dataset.kind === k;
    const textOK = !q || el.dataset.search.includes(q);
    el.classList.toggle("hidden", !(kindOK && textOK));
  }
  function refilter() { [...timeline.children].forEach(applyFilter); }

  kindSel.addEventListener("change", refilter);
  textIn.addEventListener("input", refilter);
  followBtn.addEventListener("click", () => {
    follow = !follow;
    followBtn.classList.toggle("active", follow);
    followBtn.textContent = follow ? "Follow tail" : "Paused";
  });
  document.getElementById("expand-all").addEventListener("click", () => {
    timeline.querySelectorAll(".event").forEach(e => e.classList.remove("collapsed"));
  });
  document.getElementById("collapse-all").addEventListener("click", () => {
    timeline.querySelectorAll(".event").forEach(e => e.classList.add("collapsed"));
  });

  let lastIndex = -1;

  async function loadInitial() {
    setStatus("loading events…", false);
    try {
      const resp = await fetch("/sessions/" + encodeURIComponent(SID) + "/events.json?tail=1&limit=200");
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      const data = await resp.json();
      (data.events || []).forEach(ev => { renderEvent(ev); lastIndex = Math.max(lastIndex, ev.index); });
      if ((data.events || []).length === 0 && data.total === 0) {
        emptyEl.style.display = "block";
      }
    } catch (e) {
      setStatus("load failed: " + e.message, false);
      return;
    }
    subscribe();
  }

  function subscribe() {
    const url = "/sessions/" + encodeURIComponent(SID) + "/stream?after=" + (lastIndex + 1);
    const es = new EventSource(url);
    setStatus("streaming", true);
    es.addEventListener("evt", e => {
      try {
        const ev = JSON.parse(e.data);
        renderEvent(ev);
        lastIndex = Math.max(lastIndex, ev.index);
        emptyEl.style.display = "none";
      } catch (_) {}
    });
    es.addEventListener("error", () => {
      setStatus("reconnecting…", false);
    });
    window.addEventListener("beforeunload", () => es.close());
  }

  loadInitial();
})();
</script>
{{end}}
</body>
</html>`

// notFoundHTML is the template for the 404 page.
const notFoundHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Session Not Found - workbuddy</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f6f8fa; color: #24292f; line-height: 1.5; }
  .container { max-width: 960px; margin: 0 auto; padding: 24px 16px; text-align: center; }
  h1 { margin-bottom: 8px; font-size: 24px; }
  p { color: #656d76; margin-bottom: 16px; }
  a { color: #0969da; text-decoration: none; }
  a:hover { text-decoration: underline; }
</style>
</head>
<body>
<div class="container">
  <h1>Session Not Found</h1>
  <p>No session with ID <code>{{.}}</code> was found.</p>
  <a href="/sessions">Back to sessions</a>
</div>
</body>
</html>`
