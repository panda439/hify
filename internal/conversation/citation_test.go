package conversation

import (
	"strings"
	"testing"
)

func allowed(refs ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		m[r] = struct{}{}
	}
	return m
}

func TestNormalizeCitations(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		allowed     map[string]struct{}
		wantContent string
		wantRefs    []string
		wantInvalid int
	}{
		{
			name:    "single valid citation",
			content: "答案是 42[S1]。", allowed: allowed("S1"),
			wantContent: "答案是 42[S1]。", wantRefs: []string{"S1"},
		},
		{
			name:    "multiple citations",
			content: "第一点[S1]，第二点[S2]。", allowed: allowed("S1", "S2"),
			wantContent: "第一点[S1]，第二点[S2]。", wantRefs: []string{"S1", "S2"},
		},
		{
			name:    "duplicate ref collapses to one",
			content: "[S1] 重复引用 [S1] 再来一次 [S1]", allowed: allowed("S1"),
			wantContent: "[S1] 重复引用 [S1] 再来一次 [S1]", wantRefs: []string{"S1"},
		},
		{
			name:    "illegal out of range ref stripped",
			content: "根据资料[S999]可知。", allowed: allowed("S1"),
			wantContent: "根据资料可知。", wantRefs: nil, wantInvalid: 1,
		},
		{
			name:    "S0 not a legal citation and left untouched",
			content: "看这里[S0]。", allowed: allowed("S1"),
			wantContent: "看这里[S0]。", wantRefs: nil,
		},
		{
			name:    "negative ref not a legal citation and left untouched",
			content: "看这里[S-1]。", allowed: allowed("S1"),
			wantContent: "看这里[S-1]。", wantRefs: nil,
		},
		{
			name:    "non-numeric source marker not a legal citation",
			content: "看这里[source1]。", allowed: allowed("S1"),
			wantContent: "看这里[source1]。", wantRefs: nil,
		},
		{
			name:    "no citation in answer returns empty refs",
			content: "这是一个普通回答，没有引用。", allowed: allowed("S1"),
			wantContent: "这是一个普通回答，没有引用。", wantRefs: nil,
		},
		{
			name:    "refs returned in first-occurrence order regardless of numeric order",
			content: "先说[S2]，再说[S1]，最后[S2]。", allowed: allowed("S1", "S2"),
			wantContent: "先说[S2]，再说[S1]，最后[S2]。", wantRefs: []string{"S2", "S1"},
		},
		{
			name:    "model never gets a citation it wasn't offered, even with zero evidence",
			content: "瞎编一个[S1]。", allowed: allowed(),
			wantContent: "瞎编一个。", wantRefs: nil, wantInvalid: 1,
		},
		{
			name:    "citation spanning what would have been two SSE deltas, on the fully assembled string",
			content: "答案[S" + "1]是这样的。", allowed: allowed("S1"),
			wantContent: "答案[S1]是这样的。", wantRefs: []string{"S1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotContent, gotRefs, gotInvalid := normalizeCitations(tc.content, tc.allowed)
			if gotContent != tc.wantContent {
				t.Fatalf("content = %q, want %q", gotContent, tc.wantContent)
			}
			if len(gotRefs) != len(tc.wantRefs) {
				t.Fatalf("refs = %v, want %v", gotRefs, tc.wantRefs)
			}
			for i := range gotRefs {
				if gotRefs[i] != tc.wantRefs[i] {
					t.Fatalf("refs = %v, want %v", gotRefs, tc.wantRefs)
				}
			}
			if gotInvalid != tc.wantInvalid {
				t.Fatalf("invalidCount = %d, want %d", gotInvalid, tc.wantInvalid)
			}
		})
	}
}

func TestEscapeXMLBodyPreservesNewlinesEscapesTags(t *testing.T) {
	got := escapeXMLBody("line one\nline two & <tag> \"quote\"")
	if !strings.Contains(got, "\n") {
		t.Fatalf("newline was escaped away, body text becomes unreadable: %q", got)
	}
	if strings.Contains(got, "<tag>") || !strings.Contains(got, "&lt;tag&gt;") {
		t.Fatalf("angle brackets not escaped: %q", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Fatalf("ampersand not escaped: %q", got)
	}
	// Quotes are safe unescaped inside XML text content (only attribute
	// values need quote escaping) — escapeXMLBody must not mangle them.
	if !strings.Contains(got, `"quote"`) {
		t.Fatalf("body incorrectly escaped a safe character: %q", got)
	}
}

func TestEscapeXMLAttrHandlesQuotesAndNewlines(t *testing.T) {
	got := escapeXMLAttr("evil\" break=\"out\nnewline")
	if strings.Contains(got, `"`) {
		t.Fatalf("attribute value still contains an unescaped quote, can break out of the attribute: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("attribute value still contains a literal newline, malformed XML: %q", got)
	}
}

func TestNormalizeCitationsUTF8SafeNoMangling(t *testing.T) {
	// 中文字符 + emoji 混合，确保规则删除非法引用时不破坏相邻的多字节字符。
	content := "参考资料显示🎉[S1]，另有[S99]不存在的引用，中文内容不变。"
	got, refs, invalid := normalizeCitations(content, allowed("S1"))
	want := "参考资料显示🎉[S1]，另有不存在的引用，中文内容不变。"
	if got != want {
		t.Fatalf("content = %q, want %q (utf-8 must not be mangled)", got, want)
	}
	if len(refs) != 1 || refs[0] != "S1" {
		t.Fatalf("refs = %v, want [S1]", refs)
	}
	if invalid != 1 {
		t.Fatalf("invalid = %d, want 1", invalid)
	}
}
