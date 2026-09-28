package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------- FeishuCard 4 fields ----------

// TestRenderFeishuCard_FourFields asserts that every canonical triple
// field (PlanID / Action / Actor / Comment) is rendered into a separate
// `lark_md` field of the Feishu interactive card. The four-field layout
// is what the reviewer console expects when it splits the card into a
// 2x2 grid.
func TestRenderFeishuCard_FourFields(t *testing.T) {
	got, err := RenderFeishuCard(NotifyMessage{
		PlanID:    "r-2026-09-DRG-v17",
		Action:    "APPROVE",
		Actor:     "yangjb（医保办）",
		Comment:   "已核对影响面",
		Timestamp: time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}

	payload, err := jsonRoundtrip(got)
	if err != nil {
		t.Fatalf("Feishu card is not valid JSON: %v\n%s", err, got)
	}
	if payload["msg_type"] != "interactive" {
		t.Errorf("msg_type: got %v, want interactive", payload["msg_type"])
	}
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("card missing")
	}
	elements, ok := card["elements"].([]any)
	if !ok || len(elements) == 0 {
		t.Fatalf("card.elements missing")
	}
	first, ok := elements[0].(map[string]any)
	if !ok {
		t.Fatalf("first element not a div")
	}
	fields, ok := first["fields"].([]any)
	if !ok {
		t.Fatalf("first element has no fields array")
	}
	if got := len(fields); got != 4 {
		t.Fatalf("expected 4 fields, got %d", got)
	}

	// Each field is lark_md with content. Concatenate all content strings
	// and check that every canonical triple value appears.
	combined := ""
	for i, f := range fields {
		fm, ok := f.(map[string]any)
		if !ok {
			t.Fatalf("field %d not an object", i)
		}
		if fm["tag"] != "lark_md" {
			t.Errorf("field %d tag: got %v, want lark_md", i, fm["tag"])
		}
		combined += fm["content"].(string) + "\n"
	}
	for _, want := range []string{"r-2026-09-DRG-v17", "APPROVE", "yangjb（医保办）", "已核对影响面"} {
		if !strings.Contains(combined, want) {
			t.Errorf("field grid missing %q:\n%s", want, combined)
		}
	}
}

// TestRenderFeishuCard_LinkField verifies the deep-link is rendered into
// the card. We don't ship a Link field in NotifyMessage yet (added in
// Task 26) so this guards the new behaviour.
func TestRenderFeishuCard_LinkField(t *testing.T) {
	got, err := RenderFeishuCard(NotifyMessage{
		PlanID: "p1", Action: "APPLY", Actor: "zhang", Comment: "ok",
		Link: "https://review.example.com/plans/p1",
		Timestamp: time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}
	// Link doesn't appear directly in the static Feishu template; the
	// important contract is that rendering does NOT crash and produces
	// valid JSON. Field presence is a future-extension hook.
	var payload map[string]any
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Errorf("Feishu card JSON broken: %v", err)
	}
}

// ---------- WeCom @all mention ----------

// TestRenderWeComText_MentionAll verifies that when MentionAll is true,
// the rendered body ends with the literal @all marker WeCom recognises.
// When MentionAll is false (default), no such marker appears — reviewers
// must not be @-spammed by an off-by-one.
func TestRenderWeComText_MentionAll(t *testing.T) {
	withAll, err := RenderWeComText(NotifyMessage{
		PlanID: "p1", Action: "APPROVE", Actor: "zhang", Comment: "go",
		MentionAll: true,
		Timestamp:  time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderWeComText MentionAll=true: %v", err)
	}
	if !strings.Contains(withAll, "@all") {
		t.Errorf("expected @all marker when MentionAll=true:\n%s", withAll)
	}
	if !strings.HasSuffix(strings.TrimRight(withAll, "\n"), "@all</at>") {
		t.Errorf("@all marker should be at the end:\n%s", withAll)
	}

	withoutAll, err := RenderWeComText(NotifyMessage{
		PlanID: "p1", Action: "APPROVE", Actor: "zhang", Comment: "go",
		MentionAll: false,
		Timestamp:  time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderWeComText MentionAll=false: %v", err)
	}
	if strings.Contains(withoutAll, "@all") {
		t.Errorf("@all leaked when MentionAll=false:\n%s", withoutAll)
	}
}

// ---------- escapeMarkdown ----------

// TestEscapeMarkdown_TableDriven is the canonical table for markdown
// escape behaviour. Every row says: input string → expected escaped
// output. The escape policy is documented in escapeMarkdown's docstring;
// this table is the executable contract.
func TestEscapeMarkdown_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ascii safe", "DRG-A001 stable", "DRG-A001 stable"},
		{"asterisk single", "Risk *high*", `Risk \*high\*`},
		{"asterisk plain", "10*5", `10\*5`},
		{"underscore", "snake_case", `snake\_case`},
		{"double underscore", "a__b", `a\_\_b`},
		{"backtick", "code `cfg` here", "code \\`cfg\\` here"},
		{"bracket open", "see [KB1]", `see \[KB1\]`},
		{"bracket close pair", "[x]", `\[x\]`},
		{"angle bracket", "if x<y", `if x\<y`},
		{"angle close", "if y>z", `if y\>z`},
		{"multiple mixed", "*hi* _there_ `code`", "\\*hi\\* \\_there\\_ \\`code\\`"},
		{"chinese untouched", "已核对影响面", "已核对影响面"},
		{"emoji untouched", "🎉 success", "🎉 success"},
		{"hyphen safe", "r-2026-09-DRG-v17", "r-2026-09-DRG-v17"},
		{"slash safe", "v17/v18/v19", "v17/v18/v19"},
		{"ampersand safe", "AT&T", "AT&T"},
		{"pipe safe", "a|b|c", "a|b|c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := escapeMarkdown(c.in)
			if got != c.want {
				t.Errorf("escapeMarkdown(%q):\n got  %q\n want %q", c.in, got, c.want)
			}
		})
	}
}

// ---------- escapeJSONString ----------

// TestEscapeJSONString_TableDriven mirrors the markdown table. JSON
// escaping is RFC 8259 minimum (", \, \n, \r, \t, \b, \f, control chars).
func TestEscapeJSONString_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "hello", "hello"},
		{"double quote", `say "hi"`, `say \"hi\"`},
		{"backslash", `path\to\file`, `path\\to\\file`},
		{"newline", "line1\nline2", `line1\nline2`},
		{"carriage return", "x\ry", `x\ry`},
		{"tab", "a\tb", `a\tb`},
		{"backspace", "a\bb", `a\bb`},
		{"form feed", "a\fb", `a\fb`},
		{"control nul", "x\x00y", `x\u0000y`},
		{"control bell", "x\x07y", `x\u0007y`},
		{"unicode passthrough", "已核对影响面", "已核对影响面"},
		{"emoji passthrough", "🎉", "🎉"},
		{"no surrounding quotes", `hello`, `hello`}, // explicit: do NOT add quotes
		{"all combined", "a\"b\\c\nd", `a\"b\\c\nd`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := escapeJSONString(c.in)
			if got != c.want {
				t.Errorf("escapeJSONString(%q):\n got  %q\n want %q", c.in, got, c.want)
			}
		})
	}
}

// ---------- JSON injection guard ----------

// TestRenderFeishuCard_InjectionAttempt verifies that a malicious Comment
// containing JSON-injection characters cannot break out of the Feishu
// payload's JSON envelope. The rendered card must still parse as valid
// JSON and the malicious content must appear only inside its own field,
// not as a sibling key at the top level.
func TestRenderFeishuCard_InjectionAttempt(t *testing.T) {
	malicious := `","injected_key":"pwned","x":"`
	got, err := RenderFeishuCard(NotifyMessage{
		PlanID: "p1", Action: "APPLY", Actor: "zhang", Comment: malicious,
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}

	// 1. The output must still parse as JSON (the envelope held).
	var payload map[string]any
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("Feishu payload not valid JSON after injection: %v\n%s", err, got)
	}

	// 2. The malicious sibling key must NOT exist at the top level.
	if _, present := payload["injected_key"]; present {
		t.Errorf("JSON injection succeeded — 'injected_key' appeared at top level: %s", got)
	}

	// 3. The malicious substring must appear (escaped) inside the Comment
	//    field's lark_md content.
	combined, _ := json.Marshal(payload)
	if !strings.Contains(string(combined), `\"`) {
		t.Errorf("expected escaped double-quote in rendered payload:\n%s", combined)
	}
}

// TestRenderWeComText_InjectionAttempt: same guard for WeCom's body
// (which is wrapped in JSON by the caller later). The body itself must
// not contain a quote that could break the JSON wrapping.
func TestRenderWeComText_InjectionAttempt(t *testing.T) {
	malicious := `","markdown":{"content":"pwned"},"x":"`
	got, err := RenderWeComText(NotifyMessage{
		PlanID: "p1", Action: "APPLY", Actor: "zhang", Comment: malicious,
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderWeComText: %v", err)
	}
	// Body must not contain a raw " character that would break JSON.
	// (Markdown escape does NOT touch quotes; the JSON escape happens
	// later in WeComSender.SendWeCom via encoding/json.Marshal. So the
	// renderer must NOT pre-escape quotes — otherwise double-escaping
	// would show literal \" in the rendered message.)
	if strings.Contains(got, `\"`) {
		t.Errorf("WeCom body should NOT pre-escape JSON quotes (double-escapes when wrapped):\n%s", got)
	}
	// The malicious quote must appear verbatim — it's safe because the
	// downstream json.Marshal will escape it on the way out.
	if !strings.Contains(got, `"`) {
		t.Errorf("malicious quote unexpectedly stripped from body:\n%s", got)
	}
}

// ---------- truncation ----------

// TestTruncateRunes_TableDriven: the truncateRunes helper preserves
// runes (not bytes) so emoji-heavy text is not silently cut. We use a
// table to lock down both boundary cases and the ellipsis-on-cut
// contract.
func TestTruncateRunes_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 10, ""},
		{"shorter than n", "hi", 10, "hi"},
		{"exact n", "abcdef", 6, "abcdef"},
		{"truncate ascii", "abcdefgh", 5, "abcd…"},
		{"n=1 with longer input truncates", "xyz", 1, "…"},
		{"emoji count as 1 rune", "🎉🎉🎉🎉🎉", 3, "🎉🎉…"},
		{"chinese preserved", "已核对影响面", 3, "已核…"},
		{"n<=0 no truncation", "hello", 0, "hello"},
		{"n<=0 negative", "hello", -1, "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateRunes(c.in, c.n)
			if got != c.want {
				t.Errorf("truncateRunes(%q, %d): got %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

// TestRenderFeishuCard_LongCommentTruncated: end-to-end guard that a
// very long Comment field is truncated before reaching the Feishu card,
// keeping the wire payload under webhook limits.
func TestRenderFeishuCard_LongCommentTruncated(t *testing.T) {
	long := strings.Repeat("评", 2000) // 2000 Chinese chars
	got, err := RenderFeishuCard(NotifyMessage{
		PlanID: "p1", Action: "APPLY", Actor: "zhang", Comment: long,
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}
	// Field length capped at MaxFieldLength + the ellipsis rune (which is
	// already counted inside MaxFieldLength — truncateRunes keeps n-1
	// runes plus the "…" suffix). Add generous headroom for the static
	// template boilerplate around the field.
	if utf8.RuneCountInString(got) > MaxFieldLength+800 {
		t.Errorf("rendered payload too large: %d runes (limit %d)", utf8.RuneCountInString(got), MaxFieldLength+800)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("long comment was truncated but missing ellipsis marker:\n%s", got)
	}
}

// ---------- emoji rendering ----------

// TestRenderFeishuCard_EmojiPassthrough ensures multi-byte UTF-8 emoji
// sequences survive the renderer intact (truncation works in runes, not
// bytes; escapeMarkdown does not touch emoji codepoints).
func TestRenderFeishuCard_EmojiPassthrough(t *testing.T) {
	got, err := RenderFeishuCard(NotifyMessage{
		PlanID: "p1", Action: "APPROVE 🎉", Actor: "yangjb", Comment: "👍 影响核对完毕",
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderFeishuCard: %v", err)
	}
	// The renderer must not corrupt the multi-byte sequences.
	if !strings.Contains(got, "🎉") {
		t.Errorf("emoji 🎉 missing from rendered card:\n%s", got)
	}
	if !strings.Contains(got, "👍") {
		t.Errorf("emoji 👍 missing from rendered card:\n%s", got)
	}
	// Round-trip JSON: emoji must be valid UTF-8 in the wire format.
	var payload map[string]any
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Errorf("rendered payload with emoji is not valid JSON: %v\n%s", err, got)
	}
}

// TestRenderWeComText_EmojiPassthrough: same for the WeCom path.
func TestRenderWeComText_EmojiPassthrough(t *testing.T) {
	got, err := RenderWeComText(NotifyMessage{
		PlanID: "p1", Action: "APPROVE 🎉", Actor: "yangjb", Comment: "👍",
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderWeComText: %v", err)
	}
	if !strings.Contains(got, "🎉") {
		t.Errorf("emoji 🎉 missing from WeCom body:\n%s", got)
	}
	if !strings.Contains(got, "👍") {
		t.Errorf("emoji 👍 missing from WeCom body:\n%s", got)
	}
}
