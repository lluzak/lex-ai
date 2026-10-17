// Package citation finds references to provisions in free text, e.g.
// "art. 15 ust. 1 pkt 2 ustawy o CIT" or "art. 5 § 2 k.c.", and resolves
// them to act unit paths ("art.15/ust.1/pkt.2") plus an act hint.
//
// The same resolver is used for interpretations, rulings and amending acts.
package citation

import (
	"regexp"
	"strings"
)

type Ref struct {
	Path    string // "art.15/ust.1/pkt.2"
	ActHint string // text naming the act, e.g. "ustawy o CIT"; empty if none
	Raw     string // matched text
}

const n = `\d+[a-z]{0,3}(?:\^\d+)?`

var (
	reRef = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(art\.?|§)\s*(` + n + `)` +
		`((?:\s*(?:ust\.|§)\s*` + n + `)?)` +
		`((?:\s*pkt\.?\s*` + n + `)?)` +
		`((?:\s*lit\.?\s*[a-z]{1,2}\b)?)` +
		`((?:\s*tiret\s*\d+)?)` +
		`((?:\s*(?:i|oraz|,)\s*` + n + `\b(?:\s*[a-z]\b)?)*)`)
	reNum      = regexp.MustCompile(n)
	reLitVal   = regexp.MustCompile(`(?i)lit\.?\s*([a-z]{1,2})`)
	reListItem = regexp.MustCompile(`(?:i|oraz|,)\s*(` + n + `|[a-z]{1,2})\b`)
	// Act names start with one of these words; abbreviations are handled by the alias table.
	// Act names start with one of these words and run on for a few words.
	reActName = regexp.MustCompile(`(?i)^\s*(ustaw[ayieę]|kodeks\pL*|ordynacj\pL+|konstytucj\pL+|rozporządzeni\pL+)`)
	// Abbreviations are a single token resolved through the alias table.
	reActAbbrev = regexp.MustCompile(`(?i)^\s*(k\.\s?[a-z]{1,3}\.?(?:[a-z]\.?)*|u\.\s?[a-zżźćńółęąś]\.(?:[a-zżźćńółęąś]+\.?)*|op|kc|kpc|kk|kpa|kp|ksh|uptu|updop|updof)(?:[^\p{L}]|$)`)
)

// Find returns all provision references in text, in order of appearance.
func Find(text string) []Ref {
	var refs []Ref
	for _, m := range reRef.FindAllStringSubmatchIndex(text, -1) {
		g := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return text[m[2*i]:m[2*i+1]]
		}
		kind := strings.ToLower(strings.TrimSuffix(g(1), "."))
		basic := "art"
		if kind == "§" {
			basic = "par"
		}
		segs := []string{basic + "." + g(2)}
		if s := g(3); s != "" {
			typ := "ust"
			if strings.Contains(s, "§") {
				typ = "par"
			}
			segs = append(segs, typ+"."+reNum.FindString(s))
		}
		if s := g(4); s != "" {
			segs = append(segs, "pkt."+reNum.FindString(s))
		}
		if s := g(5); s != "" {
			segs = append(segs, "lit."+strings.ToLower(reLitVal.FindStringSubmatch(s)[1]))
		}
		if s := g(6); s != "" {
			segs = append(segs, "tiret."+reNum.FindString(s))
		}
		hint := actHint(text[m[1]:])
		raw := text[m[0]:m[1]]
		refs = append(refs, Ref{Path: strings.Join(segs, "/"), ActHint: hint, Raw: strings.TrimSpace(raw)})

		// "art. 15 ust. 1 i 2" → also art.15/ust.2 (list applies to the deepest level).
		if list := g(7); list != "" {
			last := segs[len(segs)-1]
			typ := last[:strings.Index(last, ".")]
			for _, item := range reListItem.FindAllStringSubmatch(list, -1) {
				s2 := append(append([]string{}, segs[:len(segs)-1]...), typ+"."+item[1])
				refs = append(refs, Ref{Path: strings.Join(s2, "/"), ActHint: hint, Raw: strings.TrimSpace(raw)})
			}
		}
	}
	return refs
}

// actHint returns the act name that follows a reference, if any: up to the
// first bracket, semicolon or sentence end, max 12 words.
func actHint(rest string) string {
	if m := reActAbbrev.FindStringSubmatch(rest); m != nil {
		return strings.TrimRight(m[1], ".")
	}
	if !reActName.MatchString(rest) {
		return ""
	}
	s := strings.TrimSpace(rest)
	if i := strings.IndexAny(s, "(;,\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 && !isAbbrevDot(s, i) {
		s = s[:i]
	}
	words := strings.Fields(s)
	if len(words) > 12 {
		words = words[:12]
	}
	return strings.TrimRight(strings.Join(words, " "), ",. ")
}

// isAbbrevDot reports whether the dot at i belongs to a short abbreviation
// such as "k.c." or "u.p.t.u.", not a sentence end.
func isAbbrevDot(s string, i int) bool {
	start := strings.LastIndex(s[:i], " ") + 1
	return i-start <= 6 && strings.Count(s[start:i+1], ".") >= 1 && !strings.ContainsAny(s[start:i], " ")
}

// NormalizeAlias lower-cases, removes dots and collapses whitespace:
// "k.c." → "kc", "Ustawy o  CIT" → "ustawy o cit".
func NormalizeAlias(s string) string {
	s = strings.ToLower(strings.NewReplacer(".", "", ",", " ", ";", " ", ":", " ", "(", " ", ")", " ").Replace(s))
	return strings.Join(strings.Fields(s), " ")
}

// ResolveAct maps an act hint to an ELI id using the alias table. It tries the
// longest prefix of the normalized hint first, so "ustawy o podatku od towarów
// i usług (Dz.U. ...)" still resolves.
func ResolveAct(hint string, aliases map[string]string) (string, bool) {
	words := strings.Fields(NormalizeAlias(hint))
	for k := len(words); k > 0; k-- {
		if eli, ok := aliases[strings.Join(words[:k], " ")]; ok {
			return eli, true
		}
	}
	return "", false
}
