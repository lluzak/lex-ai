// Package lookup recognizes queries that point at one exact document, so the
// search box can jump straight to it instead of running a full-text search:
//
//	"Dz.U. 2024 poz. 1", "Dz. U. z 2024 r. poz. 1", "Dz.U. 1964 nr 16 poz. 93",
//	"M.P. 2023 poz. 5", "DU/2024/1", "art. 415 k.c."
package lookup

import (
	"regexp"
	"strings"

	"github.com/lluzak/lex-ai/internal/citation"
)

type Target struct {
	ELI  string // "DU/2024/1"
	Path string // optional unit path, e.g. "art.415"
}

var (
	reJournal = regexp.MustCompile(`(?i)^\s*(dz\.?\s*u\.?|m\.?\s*p\.?)\s*(?:z\s*)?(\d{4})\s*(?:r\.?)?\s*(?:nr\s*\d+\s*,?\s*)?poz\.?\s*(\d+)\s*$`)
	reELI     = regexp.MustCompile(`(?i)^\s*(DU|MP)\s*/\s*(\d{4})\s*/\s*(\d+)\s*$`)
)

// Parse returns the target of an exact query. Provision references
// ("art. 415 k.c.") need the alias table to resolve the act.
func Parse(q string, aliases map[string]string) (Target, bool) {
	if m := reJournal.FindStringSubmatch(q); m != nil {
		pub := "DU"
		if strings.HasPrefix(strings.ToLower(m[1]), "m") {
			pub = "MP"
		}
		return Target{ELI: pub + "/" + m[2] + "/" + strings.TrimLeft(m[3], "0")}, true
	}
	if m := reELI.FindStringSubmatch(q); m != nil {
		return Target{ELI: strings.ToUpper(m[1]) + "/" + m[2] + "/" + strings.TrimLeft(m[3], "0")}, true
	}
	refs := citation.Find(q)
	if len(refs) == 1 && refs[0].ActHint != "" && coversQuery(q, refs[0]) {
		if eli, ok := citation.ResolveAct(refs[0].ActHint, aliases); ok {
			return Target{ELI: eli, Path: refs[0].Path}, true
		}
	}
	return Target{}, false
}

// coversQuery makes sure the query is only a reference ("art. 415 kc"), not a
// sentence that happens to contain one.
func coversQuery(q string, r citation.Ref) bool {
	rest := strings.TrimSpace(strings.Replace(q, r.Raw, "", 1))
	return citation.NormalizeAlias(rest) == citation.NormalizeAlias(r.ActHint)
}
