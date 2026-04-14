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
const detailHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Session {{.Session.SessionID}} - workbuddy</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f6f8fa; color: #24292f; line-height: 1.5; }
  .container { max-width: 960px; margin: 0 auto; padding: 24px 16px; }
  h1 { margin-bottom: 8px; font-size: 24px; }
  .breadcrumb { margin-bottom: 16px; font-size: 14px; color: #656d76; }
  .breadcrumb a { color: #0969da; text-decoration: none; }
  .breadcrumb a:hover { text-decoration: underline; }
  .meta { background: #fff; border: 1px solid #d0d7de; border-radius: 6px; padding: 16px; margin-bottom: 16px; }
  .meta table { width: 100%; }
  .meta td { padding: 4px 12px; font-size: 14px; vertical-align: top; }
  .meta td:first-child { font-weight: 600; width: 140px; color: #656d76; }
  a { color: #0969da; text-decoration: none; }
  a:hover { text-decoration: underline; }
  code { background: #f0f3f6; padding: 2px 6px; border-radius: 4px; font-size: 13px; }
  .summary { background: #fff; border: 1px solid #d0d7de; border-radius: 6px; padding: 16px; margin-bottom: 16px; }
  .summary h2 { font-size: 18px; margin-bottom: 8px; }
  .summary pre { background: #f6f8fa; border: 1px solid #d0d7de; border-radius: 6px; padding: 12px; overflow-x: auto; font-size: 13px; white-space: pre-wrap; word-wrap: break-word; }
</style>
</head>
<body>
<div class="container">
  <div class="breadcrumb"><a href="/sessions">Sessions</a> / <code>{{.Session.SessionID}}</code></div>
  <h1>Session Detail</h1>

  <div class="meta">
    <table>
      <tr><td>Session ID</td><td><code>{{.Session.SessionID}}</code></td></tr>
      <tr><td>Task ID</td><td><code>{{.Session.TaskID}}</code></td></tr>
      <tr><td>Agent</td><td>{{.Session.AgentName}}</td></tr>
      <tr><td>Repo</td><td>{{.Session.Repo}}</td></tr>
      <tr><td>Issue</td><td>#{{.Session.IssueNum}}</td></tr>
      {{if .TaskStatus}}<tr><td>Task Status</td><td>{{.TaskStatus}}</td></tr>{{end}}
      <tr><td>Created</td><td>{{.Session.CreatedAt.Format "2006-01-02 15:04:05 UTC"}}</td></tr>
      {{if .Session.RawPath}}<tr><td>Raw File</td><td><code>{{.Session.RawPath}}</code></td></tr>{{end}}
    </table>
  </div>

  {{if .Session.Summary}}
  <div class="summary">
    <h2>Conversation Log</h2>
    <pre>{{.Session.Summary}}</pre>
  </div>
  {{end}}
</div>
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
