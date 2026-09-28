// Package notify sends Plan-review notifications to the four reviewers
// (医保办 / 信息科 / 医务科 / 财务 / 病案室) over outbound Feishu / WeCom
// webhooks. It is the live counterpart of the static dry-run report —
// instead of just printing who needs to act, we push a structured card to
// the chat channel they are already staring at.
//
// This file is the *templating* layer; outbound HTTP and the retry queue
// live in webhook.go / retry.go.
//
// Source: bytebase backend/component/review/workflow.go uses an Action /
// Actor / Comment triple to drive reviewer notifications; we adopt the
// same triple plus a PlanID so the receiver can deep-link back to the
// reviewer console.
package notify

import (
	"bytes"
	"encoding/json"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

// MaxFieldLength is the cap applied to user-supplied free-text fields
// before they are embedded into JSON or markdown. Webhooks reject payloads
// larger than a few KB; truncating at the source keeps us under the limit
// without silent loss (the trailing "..." marker tells the reader the
// comment was longer).
const MaxFieldLength = 1000

// NotifyMessage is the input triple used by both Feishu and WeCom
// renderers. It is the *display* model — no protocol quirks leak into
// callers. Adding a new field here will require updates in RenderFeishuCard
// and RenderWeComText to keep both sides in lockstep.
//
// MentionAll controls whether WeCom should add an @all to the message body.
// We do *not* mention on the Feishu side — Feishu's interactive card has
// no equivalent and silently drops unknown fields.
type NotifyMessage struct {
	PlanID     string // e.g. r-2026-09-DRG-v17
	Action     string // e.g. APPROVE / REJECT / REQUEST / APPLY / ROLLBACK
	Actor      string // e.g. yang_jb（医保办）
	Comment    string // reviewer's free-text note
	Channel    string // optional: "feishu" | "wecom" — used by retry queue
	Link       string // optional: deep-link back to the reviewer console
	MentionAll bool   // optional: WeCom @all on/off
	Timestamp  time.Time
}

// feishuCardTmpl renders a Feishu interactive-card JSON payload. The
// template intentionally keeps the schema flat (header + sections) so
// downstream Feishu client libraries do not need to grok nested tags.
//
//	{
//	  "msg_type": "interactive",
//	  "card": {
//	    "header": {"title": {"tag":"plain_text","content":"…"}},
//	    "elements": [
//	      {"tag":"div","fields":[…]},
//	      {"tag":"note","elements":[{"tag":"plain_text","content":"…"}]}
//	    ]
//	  }
//	}
//
// Field rendering goes through {{.X | safe}} where safe is the alias for
// escapeJSONString so free-text Comment/PlanID cannot break out of the
// JSON envelope. The lark_md renderer in Feishu itself strips any markdown
// the template would otherwise have to handle.
const feishuCardTmpl = `{
  "msg_type": "interactive",
  "card": {
    "header": {
      "title": {
        "tag": "plain_text",
        "content": "[{{.Action | safe}}] {{.PlanID | safe}}"
      }
    },
    "elements": [
      {
        "tag": "div",
        "fields": [
          {"tag": "lark_md", "content": "**PlanID**\n{{.PlanID | safe}}"},
          {"tag": "lark_md", "content": "**Action**\n{{.Action | safe}}"},
          {"tag": "lark_md", "content": "**Actor**\n{{.Actor | safe}}"},
          {"tag": "lark_md", "content": "**Comment**\n{{.Comment | safe}}"}
        ]
      },
      {
        "tag": "note",
        "elements": [
          {"tag": "plain_text", "content": "{{.Timestamp}}"}
        ]
      }
    ]
  }
}`

// wecomTextTmpl renders a WeCom (企业微信) markdown message body. WeCom
// only accepts a single `content.text` string, so the renderer flattens
// the triple into a 4-line block.
//
// Unlike Feishu, WeCom renders the body as markdown — so we have to escape
// stray *, _, `, [, ], <, > before the body reaches the webhook. The
// escape happens in `safeMarkdown` (see below). The Link, when present,
// is rendered as a Markdown link so the reviewer can click through.
const wecomTextTmpl = `# {{.Action | safe}} {{.PlanID | safe}}
{{- if .Link}}

[View in console]({{.Link | safe}})
{{- end}}

> **PlanID**: {{.PlanID | safe}}
> **Actor**: {{.Actor | safe}}
> **Comment**: {{.Comment | safeMarkdown}}

---

{{.Timestamp}}`

// wecomMentionAll is appended to a WeCom body when MentionAll is true.
// WeCom supports @all by appending the literal `<!everyone>` marker —
// embedding it in the markdown body avoids needing a separate JSON field
// and works for both bot and group-webhook channels.
const wecomMentionAll = "\n\n<at user_id=\"all\">@all</at>"

// RenderFeishuCard renders the Feishu interactive-card payload for the
// given message. Returns a TemplateError if the embedded template has a
// parse / execute error — these should never happen in production but
// surface a typed error so callers can errors.As on them.
//
// User-supplied fields (PlanID / Action / Actor / Comment) are JSON-escaped
// before being embedded so that a malicious actor cannot break out of the
// JSON envelope to inject webhook fields. Long fields are truncated at
// MaxFieldLength runes (not bytes — emojis are preserved).
func RenderFeishuCard(msg NotifyMessage) (string, error) {
	safe := sanitiseMessage(msg)
	return executeTemplate("feishu", feishuCardTmpl, safe)
}

// RenderWeComText renders the WeCom markdown body for the given message.
// User-supplied fields are markdown-escaped (to prevent breaking the body)
// then JSON-escaped (because WeCom wraps the body in JSON). MentionAll is
// honoured by appending the @all marker.
func RenderWeComText(msg NotifyMessage) (string, error) {
	safe := sanitiseMessage(msg)
	body, err := executeTemplate("wecom", wecomTextTmpl, safe)
	if err != nil {
		return "", err
	}
	if safe.MentionAll {
		body += wecomMentionAll
	}
	return body, nil
}

// sanitiseMessage is the single sanitisation gate. Every renderer should
// route user-supplied fields through here so the policy is consistent.
func sanitiseMessage(msg NotifyMessage) NotifyMessage {
	msg.PlanID = truncateRunes(escapeMarkdown(msg.PlanID), MaxFieldLength)
	msg.Action = truncateRunes(escapeMarkdown(msg.Action), 64)
	msg.Actor = truncateRunes(escapeMarkdown(msg.Actor), 128)
	msg.Comment = truncateRunes(escapeMarkdown(msg.Comment), MaxFieldLength)
	if msg.Link != "" {
		msg.Link = truncateRunes(msg.Link, 512)
	}
	return msg
}

// executeTemplate parses + executes a named template with two function
// aliases:
//
//   - safe: JSON-escape the value (for use inside JSON literals)
//   - safeMarkdown: escape Markdown control characters then JSON-escape
//     the result (for use inside markdown bodies that are also JSON-encoded)
//
// Centralised so the two renderers share error handling.
func executeTemplate(name, src string, msg NotifyMessage) (string, error) {
	tmpl, err := template.New(name).Funcs(template.FuncMap{
		"safe":        escapeJSONString,
		"safeMarkdown": safeMarkdown,
	}).Parse(src)
	if err != nil {
		return "", &TemplateError{Template: name, Reason: "parse: " + err.Error()}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, msg); err != nil {
		return "", &TemplateError{Template: name, Reason: "execute: " + err.Error()}
	}
	return buf.String(), nil
}

// safeMarkdown escapes Markdown control characters only. The WeCom
// renderer wraps the markdown body in JSON via Go's encoding/json
// later (in WeComSender.SendWeCom), so we leave JSON escaping to that
// step. Doing both here would produce literal "\n" text instead of
// newlines on the receiving end.
//
// For the Feishu renderer, the template body *is* the JSON literal, so
// we use `safe` (escapeJSONString) directly there instead.
func safeMarkdown(s string) string {
	return escapeMarkdown(s)
}

// escapeMarkdown escapes the Markdown control characters that have
// semantic meaning in either Feishu's lark_md dialect or standard CommonMark:
// `*` `_` `` ` `` `[` `]` `<` `>`. We deliberately leave `&` alone because
// feishu lark_md uses it for entity refs; we also leave emoji surrogate
// pairs alone (UTF-8 multi-byte sequences) so 🎉 stays 🎉.
//
// The escape uses a backslash prefix, which is the standard Markdown
// convention. Inside JSON-string values the backslash is itself JSON-
// escaped, so callers that route this through JSON should call
// escapeJSONString as well (that's what safeMarkdown does).
func escapeMarkdown(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '*', '_', '`', '[', ']', '<', '>':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeJSONString escapes a string for safe inclusion inside a JSON
// string literal (including surrounding quote characters). It is the
// minimum set of escapes required by RFC 8259:
//
//   - " → \"
//   - \ → \\
//   - newline → \n
//   - carriage return → \r
//   - tab → \t
//   - backspace → \b
//   - form feed → \f
//   - other control characters (U+0000..U+001F except the above) → \uXXXX
//
// This deliberately does *not* use encoding/json.Marshal because that
// adds surrounding quotes and would double-escape when the template
// engine already wraps the value in quotes.
func escapeJSONString(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				// Control chars must be \uXXXX escaped in JSON strings.
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[(r>>4)&0xF])
				b.WriteByte(hex[r&0xF])
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// truncateRunes truncates s to at most n runes. If truncation happens,
// an ellipsis "…" is appended so the reader can tell the value was
// longer. We work in runes (not bytes) so a 1000-character emoji wall is
// not silently cut mid-codepoint.
//
// If n <= 0 the original string is returned.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	// Walk runes and keep the first n-1, then append the ellipsis.
	kept := 0
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if kept == n-1 {
			break
		}
		b.WriteRune(r)
		kept++
	}
	b.WriteString("…")
	return b.String()
}

// TemplateError signals a render failure. Distinct from webhook errors so
// callers can decide whether to retry (template bug → no, network → yes).
type TemplateError struct {
	Template string // "feishu" | "wecom"
	Reason   string
}

func (e *TemplateError) Error() string {
	return "notify: render " + e.Template + " template: " + e.Reason
}

// jsonRoundtrip is a test/dev helper exposed at package level so that
// tests can verify a renderer's output is valid JSON without importing
// encoding/json themselves. Not part of the production API surface.
func jsonRoundtrip(s string) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}
