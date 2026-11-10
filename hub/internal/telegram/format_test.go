package telegram_test

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

func units(s string) int { return len(utf16.Encode([]rune(s))) }

func TestRenderMarkdownInline(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain <b> & \"q\"":       "plain &lt;b&gt; &amp; &#34;q&#34;",
		"**bold** and `a<b` code": "<b>bold</b> and <code>a&lt;b</code> code",
		"# Title":                 "<b>Title</b>",
		"see [docs](https://x.example/a?b=c&d=e)": `see <a href="https://x.example/a?b=c&amp;d=e">docs</a>`,
		"[bad](javascript:alert(1))":              "[bad](javascript:alert(1))",
		"odd ` backtick":                          "odd ` backtick",
		"```go\nfunc main() {}\n```":              `<pre><code class="language-go">func main() {}</code></pre>`,
		"```\nunclosed <x>":                       "<pre><code>unclosed &lt;x&gt;</code></pre>",
	}
	for in, want := range cases {
		got := telegram.RenderMarkdown(in, telegram.MaxMessageLen)
		if len(got) != 1 || got[0] != want {
			t.Errorf("RenderMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderMarkdownSplitsCodeBlocks(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("intro\n```sh\n")
	for range 500 {
		b.WriteString("echo 'line with <angle> & amp'\n")
	}
	b.WriteString("```\noutro")
	chunks := telegram.RenderMarkdown(b.String(), 1000)
	if len(chunks) < 10 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	lines := 0
	for i, c := range chunks {
		if units(c) > 1000 {
			t.Fatalf("chunk %d is %d units", i, units(c))
		}
		if strings.Count(c, "<pre>") != strings.Count(c, "</pre>") {
			t.Fatalf("chunk %d has unbalanced pre: %q", i, c)
		}
		lines += strings.Count(c, "echo")
	}
	if lines != 500 {
		t.Fatalf("got %d code lines, want 500", lines)
	}
	if !strings.HasPrefix(chunks[0], "intro") || !strings.HasSuffix(chunks[len(chunks)-1], "outro") {
		t.Fatalf("lost prose around the code block")
	}
}

func TestSplitCountsUTF16(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("😀", 5000)
	chunks := telegram.SplitPlain(text, telegram.MaxMessageLen)
	total := 0
	for _, c := range chunks {
		if units(c) > telegram.MaxMessageLen {
			t.Fatalf("chunk is %d units", units(c))
		}
		total += len([]rune(c))
	}
	if total != 5000 || len(chunks) != 3 {
		t.Fatalf("got %d runes in %d chunks", total, len(chunks))
	}
}

func TestLongProseLineIsHardSplit(t *testing.T) {
	t.Parallel()
	chunks := telegram.RenderMarkdown(strings.Repeat("&", 3000), telegram.MaxMessageLen)
	total := 0
	for _, c := range chunks {
		if units(c) > telegram.MaxMessageLen {
			t.Fatalf("chunk is %d units", units(c))
		}
		total += strings.Count(c, "&amp;")
	}
	if total != 3000 {
		t.Fatalf("got %d ampersands", total)
	}
}
