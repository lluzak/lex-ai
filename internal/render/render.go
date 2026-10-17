// Package render turns stored documents into the HTML page body that is
// saved in document.rendered_html. Pages are rendered once per change, not
// per request; the web layer only wraps the body in the site layout.
package render

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"strings"

	"github.com/lluzak/lex-ai/internal/store"
)

//go:embed templates/*.html
var templates embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"label":     UnitLabel,
	"isHeading": isHeading,
	"date":      store.FormatDate,
}).ParseFS(templates, "templates/*.html"))

type ActPage struct {
	Doc       store.Document
	Units     []store.StoredUnit
	Keywords  []string
	SourceURL string // official source (ISAP / ELI)
	PDFURL    string // our copy of the original PDF on the files domain, if public
	TOC       []store.StoredUnit
}

// Act renders the body of an act page and returns it with its ETag.
func Act(p ActPage) (string, string, error) {
	for _, u := range p.Units {
		if isHeading(u.Type) || (u.Type == "art" || (u.Type == "par" && u.Depth == 0)) {
			p.TOC = append(p.TOC, u)
		}
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "act.html", p); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.String(), `"` + hex.EncodeToString(sum[:8]) + `"`, nil
}

func isHeading(t string) bool {
	switch t {
	case "czesc", "ksiega", "tytul", "dzial", "rozdzial", "oddzial":
		return true
	}
	return false
}

var unitNames = map[string]string{
	"czesc": "Część", "ksiega": "Księga", "tytul": "Tytuł", "dzial": "Dział",
	"rozdzial": "Rozdział", "oddzial": "Oddział", "art": "Art.", "par": "§",
	"ust": "", "pkt": "", "lit": "", "tiret": "–",
}

// UnitLabel is how a unit's number is printed: "Art. 15.", "§ 2.", "1.", "2)", "a)", "–".
func UnitLabel(typ, number string) string {
	n := SupNumber(number)
	switch typ {
	case "art", "par":
		return unitNames[typ] + " " + n + "."
	case "ust":
		return n + "."
	case "pkt", "lit":
		return n + ")"
	case "tiret":
		return "–"
	default:
		return unitNames[typ] + " " + n
	}
}

// SupNumber prints "4^1" as "4¹".
func SupNumber(n string) string {
	i := strings.Index(n, "^")
	if i < 0 {
		return n
	}
	return n[:i] + strings.NewReplacer("0", "⁰", "1", "¹", "2", "²", "3", "³", "4", "⁴",
		"5", "⁵", "6", "⁶", "7", "⁷", "8", "⁸", "9", "⁹").Replace(n[i+1:])
}
