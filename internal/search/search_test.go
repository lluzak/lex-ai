package search

import "testing"

func TestMarkSnippetEscapes(t *testing.T) {
	got := markSnippet("a <script> " + selStart + "szkoda" + selStop + " & b")
	want := "a &lt;script&gt; <mark>szkoda</mark> &amp; b"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
