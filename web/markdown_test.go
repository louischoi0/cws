package main

import (
	"strings"
	"testing"
)

// Task, issue and result bodies are written by agents and by anything that
// can POST to the API — untrusted input by the time it reaches this page.
// Rendering it as markdown is only safe while goldmark's raw-HTML and
// URL-scheme handling stay at their defaults, so this pins both. It fails
// the moment someone adds html.WithUnsafe() to the renderer in main.go.
func TestMarkdownNeutralizesUntrustedInput(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"script tag", `<script>alert(1)</script>`},
		{"img onerror", `<img src=x onerror=alert(1)>`},
		{"svg onload", `<svg/onload=alert(1)>`},
		{"iframe", `<iframe src="//evil"></iframe>`},
		{"inline handler", `<div onclick="alert(1)">x</div>`},
		{"raw anchor", `<a href="javascript:alert(1)">c</a>`},
		{"markdown js link", `[click](javascript:alert(1))`},
		{"markdown js link, mixed case", `[click](JaVaScRiPt:alert(1))`},
		{"markdown js image", `![x](javascript:alert(1))`},
		{"data uri link", `[click](data:text/html;base64,PHNjcmlwdD4=)`},
	}

	// Any of these appearing in the output means a live element or a live
	// URL survived. Escaped text (&lt;script&gt;) is fine and expected.
	forbidden := []string{
		"<script", "<img src=x", "<iframe", "<svg", "<div onclick",
		"javascript:", "data:text/html",
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.ToLower(string(renderMarkdown(c.in)))
			for _, bad := range forbidden {
				if strings.Contains(got, bad) {
					t.Errorf("rendered %q live in output:\n  in:  %s\n  out: %s", bad, c.in, got)
				}
			}
		})
	}
}

func TestMarkdownRendersOrdinaryContent(t *testing.T) {
	// The shapes task bodies actually use: headings, emphasis, inline code,
	// fenced code, lists, links, and a GFM table.
	src := "# Heading\n\n**bold** and `inline`\n\n```go\nx := 1\n```\n\n" +
		"- one\n- two\n\n[link](https://example.com)\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	got := string(renderMarkdown(src))

	for _, want := range []string{
		"<h1", "<strong>", "<code>", "<pre>", "<ul>", "<li>",
		`<a href="https://example.com"`, "<table>", "<td>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in rendered output, got:\n%s", want, got)
		}
	}
}

// A body that fails to convert must still reach the reader, escaped,
// rather than vanishing.
func TestMarkdownFallbackEscapes(t *testing.T) {
	got := string(renderMarkdown("plain <b>text</b>"))
	if strings.Contains(got, "<b>") {
		t.Errorf("raw tag survived: %s", got)
	}
}
