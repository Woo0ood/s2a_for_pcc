package service

import (
	"bytes"
	"html/template"
	"io"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared template fragments (used by both in-memory and streaming renderers)
// ─────────────────────────────────────────────────────────────────────────────

// transcriptHeadHTML is the fixed opening of every transcript HTML document.
// It contains <!DOCTYPE html> … through the <h1> header (including all <style>).
const transcriptHeadHTML = `<!DOCTYPE html>
<html lang="zh-Hans">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Payload Audit Transcript</title>
<style>
/* ── reset & base ── */
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  font-size: 14px; line-height: 1.6;
  background: #f5f5f5; color: #222; padding: 24px;
}
h1 { font-size: 20px; font-weight: 700; margin-bottom: 16px; }
h2 { font-size: 16px; font-weight: 600; margin-bottom: 8px; }

/* ── manifest card ── */
.manifest {
  background: #fff; border: 1px solid #e0e0e0; border-radius: 8px;
  padding: 20px; margin-bottom: 24px;
  box-shadow: 0 1px 3px rgba(0,0,0,.08);
}
.manifest-meta { display: flex; flex-wrap: wrap; gap: 12px 24px; margin-bottom: 16px; }
.manifest-meta dt { font-weight: 600; color: #555; font-size: 12px; text-transform: uppercase; }
.manifest-meta dd { font-size: 14px; word-break: break-all; }
.gaps-list {
  background: #fff8e1; border-left: 4px solid #f9a825;
  border-radius: 4px; padding: 12px 16px; margin-top: 12px;
}
.gaps-list h3 { font-size: 13px; font-weight: 700; color: #e65100; margin-bottom: 8px; }
.gaps-list ul { list-style: none; padding: 0; }
.gaps-list li {
  padding: 4px 0; color: #bf360c; font-size: 13px;
  border-bottom: 1px dashed #ffcc80;
}
.gaps-list li:last-child { border-bottom: none; }
.gaps-list li::before { content: "⚠ "; }

/* ── turn block ── */
.turn {
  background: #fff; border: 1px solid #e0e0e0; border-radius: 8px;
  margin-bottom: 16px; overflow: hidden;
  box-shadow: 0 1px 3px rgba(0,0,0,.06);
}
.turn-header {
  background: #f8f8f8; border-bottom: 1px solid #e8e8e8;
  padding: 8px 16px; display: flex; flex-wrap: wrap;
  align-items: center; gap: 8px 16px; font-size: 12px; color: #666;
}
.turn-header strong { font-size: 14px; color: #333; }
.badge {
  display: inline-block; padding: 2px 8px; border-radius: 12px;
  font-size: 11px; font-weight: 600; background: #e3f2fd; color: #1565c0;
}
.badge.status-ok { background: #e8f5e9; color: #2e7d32; }
.badge.status-err { background: #ffebee; color: #c62828; }

.turn-body { padding: 16px; }
.section { margin-bottom: 16px; }
.section:last-child { margin-bottom: 0; }

/* ── item ── */
.item { margin-bottom: 10px; }
.item:last-child { margin-bottom: 0; }
.item-label {
  font-size: 11px; font-weight: 700; text-transform: uppercase;
  color: #888; margin-bottom: 4px; letter-spacing: .05em;
}
.item-text {
  background: #fafafa; border: 1px solid #ececec; border-radius: 4px;
  padding: 8px 12px; white-space: pre-wrap; word-break: break-word;
  font-size: 13px; line-height: 1.55;
}
.item-text.user      { border-left: 3px solid #1976d2; }
.item-text.assistant { border-left: 3px solid #388e3c; }
.item-text.system    { border-left: 3px solid #f57c00; }
.item-text.developer { border-left: 3px solid #7b1fa2; }

/* ── per-item role chip ── */
.role-chip {
  display: inline-block; padding: 2px 7px; border-radius: 10px;
  font-size: 10px; font-weight: 700; letter-spacing: .04em;
  text-transform: uppercase; margin-bottom: 4px;
}
.role-chip.system    { background: #fff3e0; color: #e65100; }
.role-chip.developer { background: #f3e5f5; color: #6a1b9a; }
.role-chip.user      { background: #e3f2fd; color: #1565c0; }
.role-chip.other     { background: #f5f5f5; color: #555; }

/* ── collapsible details ── */
details { margin-bottom: 8px; }
details summary {
  cursor: pointer; font-size: 13px; font-weight: 600;
  padding: 6px 10px; background: #f0f4ff; border-radius: 4px;
  user-select: none; color: #1a237e;
}
details summary:hover { background: #e3eaff; }
details[open] summary { border-radius: 4px 4px 0 0; }
.details-body {
  border: 1px solid #dde3f7; border-top: none; border-radius: 0 0 4px 4px;
  padding: 10px 12px; font-size: 13px;
  white-space: pre-wrap; word-break: break-word;
  background: #fafbff;
}

/* ── attachments ── */
.attachments { margin-top: 8px; }
.attachment-link {
  display: inline-flex; align-items: center; gap: 4px;
  padding: 4px 10px; background: #e8f5e9; color: #2e7d32;
  border-radius: 4px; text-decoration: none; font-size: 12px;
  border: 1px solid #c8e6c9; margin: 2px 4px 2px 0;
}
.attachment-link:hover { background: #c8e6c9; }
.attachment-img {
  display: inline-block; margin: 4px 0;
}
.attachment-img figcaption {
  font-size: 11px; color: #888; margin-top: 4px;
}

/* ── meta footer ── */
.turn-footer {
  padding: 6px 16px; background: #f8f8f8; border-top: 1px solid #eee;
  font-size: 11px; color: #999; display: flex; gap: 16px; flex-wrap: wrap;
}
</style>
</head>
<body>

<h1>📋 Payload Audit — Conversation Transcript</h1>
`

// transcriptManifestTemplateStr is the manifest <section> block.
// It executes against a Manifest value directly.
const transcriptManifestTemplateStr = `<section class="manifest">
  <h2>📊 完整性 Manifest</h2>
  <dl class="manifest-meta">
    <div>
      <dt>Conversation Key</dt>
      <dd>{{if .ConversationKey}}{{.ConversationKey}}{{else}}<em>（单轮副本）</em>{{end}}</dd>
    </div>
    <div>
      <dt>Turn Count</dt>
      <dd>{{.TurnCount}}</dd>
    </div>
    <div>
      <dt>Time From</dt>
      <dd>{{formatTime .TimeFrom}}</dd>
    </div>
    <div>
      <dt>Time To</dt>
      <dd>{{formatTime .TimeTo}}</dd>
    </div>
  </dl>
  {{if .Gaps}}
  <div class="gaps-list">
    <h3>⚠ 缺口声明 (Completeness Gaps)</h3>
    <ul>
      {{range .Gaps}}<li>{{.}}</li>{{end}}
    </ul>
  </div>
  {{else}}
  <p style="color:#2e7d32;font-size:13px;">✅ 无已知缺口</p>
  {{end}}
</section>
`

// transcriptTurnTemplateStr is the single <article> block for one Turn.
// It executes against a Turn value directly.
const transcriptTurnTemplateStr = `<article class="turn">
  <header class="turn-header">
    <strong>Turn {{.Index}}</strong>
    <span>{{formatTime .CreatedAt}}</span>
    {{if .Model}}<span class="badge">{{.Model}}</span>{{end}}
    {{if .StatusCode}}
    <span class="badge {{if lt .StatusCode 400}}status-ok{{else}}status-err{{end}}">HTTP {{.StatusCode}}</span>
    {{end}}
  </header>
  <div class="turn-body">

    {{/* User / Input items */}}
    {{if .UserItems}}
    <section class="section">
      <h2>💬 Input (本轮新增)</h2>
      {{range .UserItems}}
      <div class="item">
        {{if eq .Type "message"}}
          {{/* Role chip: system / developer / user / (other) */}}
          {{if eq .Role "system"}}
            <span class="role-chip system">⚙️ System</span>
          {{else if eq .Role "developer"}}
            <span class="role-chip developer">🧑‍💻 Developer</span>
          {{else if eq .Role "user"}}
            <span class="role-chip user">👤 User</span>
          {{else}}
            <span class="role-chip other">{{if .Role}}{{.Role}}{{else}}user{{end}}</span>
          {{end}}
          <div class="item-text {{if .Role}}{{.Role}}{{else}}user{{end}}">{{.Text}}</div>
        {{else if eq .Type "function_call_output"}}
          <details>
            <summary>📄 Tool Output{{if .ToolName}}: {{.ToolName}}{{end}}</summary>
            <div class="details-body">{{.ToolOutput}}</div>
          </details>
        {{else if eq .Type "function_call"}}
          <details>
            <summary>🔧 Tool Call: {{.ToolName}}</summary>
            <div class="details-body">{{.ToolArgs}}</div>
          </details>
        {{else if eq .Type "reasoning"}}
          <details>
            <summary>💭 Reasoning</summary>
            <div class="details-body">{{.Text}}</div>
          </details>
        {{else if eq .Type "tool_search_call"}}
          <details>
            <summary>🔍 Search Call{{if .ToolName}}: {{.ToolName}}{{end}}</summary>
            <div class="details-body">{{if .Text}}{{.Text}}{{else}}(no query text){{end}}</div>
          </details>
        {{else if eq .Type "tool_search_output"}}
          <details>
            <summary>📋 Search Output{{if .ToolName}}: {{.ToolName}}{{end}}</summary>
            <div class="details-body">{{if .ToolOutput}}{{.ToolOutput}}{{else}}(no output){{end}}</div>
          </details>
        {{else}}
          <div class="item-label">raw</div>
          <div class="item-text">{{.Text}}</div>
        {{end}}
      </div>
      {{end}}
    </section>
    {{end}}

    {{/* Assistant / Output items */}}
    {{if .Assistant}}
    <section class="section">
      <h2>🤖 Output</h2>
      {{range .Assistant}}
      <div class="item">
        {{if eq .Type "message"}}
          <div class="item-label">assistant</div>
          <div class="item-text assistant">{{.Text}}</div>
        {{else if eq .Type "function_call"}}
          <details>
            <summary>🔧 {{.ToolName}}</summary>
            <div class="details-body">{{.ToolArgs}}</div>
          </details>
        {{else if eq .Type "function_call_output"}}
          <details>
            <summary>📄 Tool Output</summary>
            <div class="details-body">{{.ToolOutput}}</div>
          </details>
        {{else if eq .Type "reasoning"}}
          <details>
            <summary>💭 Reasoning</summary>
            <div class="details-body">{{.Text}}</div>
          </details>
        {{else}}
          <div class="item-label">raw</div>
          <div class="item-text">{{.Text}}</div>
        {{end}}
      </div>
      {{end}}
    </section>
    {{end}}

    {{/* Attachments */}}
    {{if .Attachments}}
    <section class="section attachments">
      <h2>🖼 Attachments</h2>
      {{range .Attachments}}
      {{if .DataURI}}
        <figure class="attachment-img">
          <img src="{{ safeURL .DataURI }}" alt="blob {{ .SHA256 }}" style="max-width:100%;height:auto;border:1px solid #ccc" />
          <figcaption>{{ .MIME }} · {{ .Bytes }} bytes · {{ .SHA256 }}</figcaption>
        </figure>
      {{else}}
        <a class="attachment-link" href="{{ .ProxyPath }}">🖼 {{ .MIME }} · {{ .Bytes }} bytes · {{ .SHA256 }}</a>
      {{end}}
      {{end}}
    </section>
    {{end}}

  </div>
  <footer class="turn-footer">
    <span>Input: {{.RawInputBytes}} bytes</span>
    <span>Output: {{.RawOutputBytes}} bytes</span>
  </footer>
</article>
`

// transcriptFooterHTML closes the HTML document.
const transcriptFooterHTML = `</body></html>`

// ─────────────────────────────────────────────────────────────────────────────
// funcMap shared by all renderers
// ─────────────────────────────────────────────────────────────────────────────

func transcriptFuncMap() template.FuncMap {
	return template.FuncMap{
		"formatTime": func(ts time.Time) string {
			if ts.IsZero() {
				return "—"
			}
			return ts.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		// safeURL marks a data: URI as trusted so html/template does not rewrite it
		// to #ZgotmplZ. Only used for validated image data URIs we generated ourselves.
		"safeURL": func(s string) template.URL { return template.URL(s) },
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RenderTranscriptHTML
// ─────────────────────────────────────────────────────────────────────────────

// RenderTranscriptHTML renders a Transcript as a self-contained HTML page.
// All user-supplied content is auto-escaped by html/template; no raw injection possible.
// The output has no external JS, no external CSS, no presigned URLs.
func RenderTranscriptHTML(t Transcript) ([]byte, error) {
	funcMap := transcriptFuncMap()

	manifestTmpl, err := template.New("manifest").Funcs(funcMap).Parse(transcriptManifestTemplateStr)
	if err != nil {
		return nil, err
	}
	turnTmpl, err := template.New("turn").Funcs(funcMap).Parse(transcriptTurnTemplateStr)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	// Head (static string).
	if _, err := io.WriteString(&buf, transcriptHeadHTML); err != nil {
		return nil, err
	}

	// Manifest card at top.
	if err := manifestTmpl.Execute(&buf, t.Manifest); err != nil {
		return nil, err
	}

	// Turn articles.
	if len(t.Turns) > 0 {
		for _, turn := range t.Turns {
			if err := turnTmpl.Execute(&buf, turn); err != nil {
				return nil, err
			}
		}
	} else {
		// Preserve the {{else}} empty-case from the original template.
		if _, err := io.WriteString(&buf, "\n<p style=\"color:#999;font-style:italic;\">No turns in this conversation.</p>\n"); err != nil {
			return nil, err
		}
	}

	// Footer (static string).
	if _, err := io.WriteString(&buf, "\n"+transcriptFooterHTML); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
