package htmltext

import (
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	doc := `<html><head><style>p{}</style></head><body>
<h1>USTAWA</h1>
<p>Art. 4<sup>1</sup>.&nbsp;Przepis   szczególny</p>
<div><p>1) pierwszy;</p><p>2) drugi.</p></div>
<script>alert(1)</script>
</body></html>`
	got, err := Lines([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"USTAWA", "Art. 4^1. Przepis szczególny", "1) pierwszy;", "2) drugi."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}
