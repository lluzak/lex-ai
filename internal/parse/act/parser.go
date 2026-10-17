// Package act parses the text of a Polish legal act into its structural units
// following the rules of legislative technique (Zasady techniki prawodawczej):
//
//	część → księga → tytuł → dział → rozdział → oddział      (headings)
//	art. → ust. (or § in codes) → pkt → lit. → tiret         (provisions)
//
// Regulations (rozporządzenia) use § as the basic unit instead of art.
//
// Input is a slice of text lines (from HTML or PDF extraction). The parser is
// deliberately forgiving: anything it doesn't recognize becomes continuation
// text of the current unit, so no text is ever lost.
package act

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version is stored with each parsed document. Bump it when parsing output
// changes so affected documents can be re-parsed.
const Version = 1

type Unit struct {
	Type    string // czesc, ksiega, tytul, dzial, rozdzial, oddzial, art, par, ust, pkt, lit, tiret
	Number  string // "15a", "4^1", "II", "a"; tirets are numbered 1, 2, ...
	Path    string // "art.15/ust.1/pkt.2"; headings: "dzial.II/rozdzial.1"
	Heading string // heading title, e.g. "Przepisy ogólne"
	Text    string
	Depth   int
	Parent  int // index into Result.Units, -1 for top level
}

type Result struct {
	Preamble string // title and preamble before the first unit
	Units    []Unit
}

const (
	rankCzesc = iota + 1
	rankKsiega
	rankTytul
	rankDzial
	rankRozdzial
	rankOddzial
	rankBasic // art. (or § in regulations)
	rankUst   // ust. (or § inside an article)
	rankPkt
	rankLit
	rankTiret
)

var headingRanks = map[string]int{
	"czesc": rankCzesc, "ksiega": rankKsiega, "tytul": rankTytul,
	"dzial": rankDzial, "rozdzial": rankRozdzial, "oddzial": rankOddzial,
}

const num = `(\d+[a-z]{0,3}(?:\^\d+[a-z]{0,2})?)`

var (
	reHeading = regexp.MustCompile(`^(?i)(CZĘŚĆ|KSIĘGA|TYTUŁ|DZIAŁ|ROZDZIAŁ|ODDZIAŁ)\s+([IVXLCDM]+[a-z]{0,3}|\d+[a-z]{0,3}|[A-ZĄĆĘŁŃÓŚŹŻ]+[A-ZĄĆĘŁŃÓŚŹŻa-ząćęłńóśźż]*)\.?(?:\s+(.*))?$`)
	reArt     = regexp.MustCompile(`^Art\.\s*` + num + `\.\s*(.*)$`)
	rePar     = regexp.MustCompile(`^§\s*` + num + `\.\s*(.*)$`)
	reUst     = regexp.MustCompile(`^` + num + `\.\s+(.*)$`)
	rePkt     = regexp.MustCompile(`^` + num + `\)\s*(.*)$`)
	reLit     = regexp.MustCompile(`^([a-z]{1,2}(?:\^\d+)?)\)\s*(.*)$`)
	reTiret   = regexp.MustCompile(`^[–—-]\s+(.+)$`)
	reArtScan = regexp.MustCompile(`^Art\.\s*\d`)
)

var headingTypes = map[string]string{
	"część": "czesc", "księga": "ksiega", "tytuł": "tytul",
	"dział": "dzial", "rozdział": "rozdzial", "oddział": "oddzial",
}

// ordinal words used for "CZĘŚĆ OGÓLNA", "KSIĘGA PIERWSZA" etc.
var wordHeadingOK = regexp.MustCompile(`^(?i)(ogólna|szczególna|wojskowa|pierwsza|druga|trzecia|czwarta|piąta|szósta|siódma|ósma|dziewiąta|dziesiąta|[IVXLCDM]+[a-z]{0,3}|\d+[a-z]{0,3})$`)

type open struct {
	idx  int
	rank int
}

type parser struct {
	res          Result
	stack        []open
	artMode      bool // the act uses "Art." as its basic unit
	quoteDepth   int
	pendingTitle int // index of a heading waiting for its title line, -1 if none
	tiretCount   map[int]int
	preamble     []string
}

// Parse splits lines into structural units.
func Parse(lines []string) Result {
	p := &parser{pendingTitle: -1, tiretCount: map[int]int{}}
	lines = normalize(lines)
	p.artMode = scanArtMode(lines)

	for _, line := range lines {
		inQuote := p.quoteDepth > 0
		p.quoteDepth += quoteDelta(line)
		if p.quoteDepth < 0 {
			p.quoteDepth = 0
		}
		if inQuote || !p.structural(line) {
			p.continuation(line)
		}
	}
	p.res.Preamble = strings.Join(p.preamble, "\n")
	return p.res
}

func normalize(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = normalizeSuperscripts(strings.TrimSpace(l))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

var superscripts = strings.NewReplacer(
	"⁰", "0", "¹", "1", "²", "2", "³", "3", "⁴", "4",
	"⁵", "5", "⁶", "6", "⁷", "7", "⁸", "8", "⁹", "9",
)

// normalizeSuperscripts rewrites Unicode superscript digits after a number
// ("Art. 15a¹.") to the caret form used by the HTML extractor ("Art. 15a^1.").
func normalizeSuperscripts(l string) string {
	if !strings.ContainsAny(l, "⁰¹²³⁴⁵⁶⁷⁸⁹") {
		return l
	}
	var b strings.Builder
	inSup := false
	for _, r := range l {
		if strings.ContainsRune("⁰¹²³⁴⁵⁶⁷⁸⁹", r) {
			if !inSup {
				b.WriteByte('^')
				inSup = true
			}
			b.WriteString(superscripts.Replace(string(r)))
			continue
		}
		inSup = false
		b.WriteRune(r)
	}
	return b.String()
}

func scanArtMode(lines []string) bool {
	depth := 0
	for _, l := range lines {
		if depth == 0 && reArtScan.MatchString(l) {
			return true
		}
		depth += quoteDelta(l)
		if depth < 0 {
			depth = 0
		}
	}
	return false
}

// quoteDelta counts Polish opening („) minus closing (” or “) quotation marks.
// Amending acts quote whole provisions of the amended act; those must not be
// parsed as units of the amending act.
func quoteDelta(l string) int {
	return strings.Count(l, "„") - strings.Count(l, "”") - strings.Count(l, "“")
}

func (p *parser) structural(line string) bool {
	if m := reHeading.FindStringSubmatch(line); m != nil {
		typ := headingTypes[strings.ToLower(m[1])]
		if !wordHeadingOK.MatchString(m[2]) {
			return false
		}
		// "KSIĘGA PIERWSZA" followed by "CZĘŚĆ OGÓLNA": an outer heading word
		// right after a heading is that heading's title, not a new unit.
		if p.pendingTitle >= 0 && headingRanks[typ] <= headingRanks[p.res.Units[p.pendingTitle].Type] {
			return false
		}
		idx := p.push(typ, m[2], headingRanks[typ], "")
		if title := strings.TrimSpace(m[3]); title != "" {
			p.res.Units[idx].Heading = title
		} else {
			p.pendingTitle = idx
		}
		return true
	}
	if m := reArt.FindStringSubmatch(line); m != nil && p.artMode {
		p.push("art", m[1], rankBasic, "")
		p.inline(m[2])
		return true
	}
	if m := rePar.FindStringSubmatch(line); m != nil {
		if p.artMode {
			if !p.within(rankBasic) {
				return false
			}
			p.push("par", m[1], rankUst, "")
		} else {
			p.push("par", m[1], rankBasic, "")
		}
		p.inline(m[2])
		return true
	}
	if m := reUst.FindStringSubmatch(line); m != nil && p.within(rankBasic) && p.allowsUst() {
		p.push("ust", m[1], rankUst, m[2])
		return true
	}
	if m := rePkt.FindStringSubmatch(line); m != nil && p.within(rankBasic) {
		p.push("pkt", m[1], rankPkt, m[2])
		return true
	}
	if m := reLit.FindStringSubmatch(line); m != nil && p.within(rankPkt) {
		p.push("lit", m[1], rankLit, m[2])
		return true
	}
	if m := reTiret.FindStringSubmatch(line); m != nil && p.within(rankLit) {
		parent := p.parentFor(rankTiret)
		p.tiretCount[parent]++
		p.push("tiret", itoa(p.tiretCount[parent]), rankTiret, m[1])
		return true
	}
	return false
}

// inline handles units that start on the same line as their parent:
// "Art. 15. 1. Kosztami ..." or "Art. 5. § 1. ..." or "§ 1. 1. ...".
func (p *parser) inline(rest string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return
	}
	top := p.stack[len(p.stack)-1]
	if top.rank == rankBasic {
		if m := rePar.FindStringSubmatch(rest); m != nil && p.artMode {
			p.push("par", m[1], rankUst, "")
			p.appendText(p.stack[len(p.stack)-1].idx, m[2])
			return
		}
		if m := reUst.FindStringSubmatch(rest); m != nil {
			p.push("ust", m[1], rankUst, m[2])
			return
		}
	}
	p.appendText(top.idx, rest)
}

// allowsUst prevents "1." lines inside a § of a code article being read as ust.
func (p *parser) allowsUst() bool {
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].rank == rankUst {
			return p.res.Units[p.stack[i].idx].Type != "par"
		}
	}
	return true
}

// within reports whether a unit of the given rank is currently open.
func (p *parser) within(rank int) bool {
	for _, o := range p.stack {
		if o.rank == rank {
			return true
		}
	}
	return false
}

// parentFor returns the index of the unit a new unit of the given rank would
// be attached to, without modifying the stack.
func (p *parser) parentFor(rank int) int {
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].rank < rank {
			return p.stack[i].idx
		}
	}
	return -1
}

func (p *parser) push(typ, number string, rank int, text string) int {
	for len(p.stack) > 0 && p.stack[len(p.stack)-1].rank >= rank {
		p.stack = p.stack[:len(p.stack)-1]
	}
	parent := -1
	if len(p.stack) > 0 {
		parent = p.stack[len(p.stack)-1].idx
	}
	u := Unit{
		Type:   typ,
		Number: number,
		Text:   strings.TrimSpace(text),
		Depth:  len(p.stack),
		Parent: parent,
		Path:   p.path(parent, typ, number, rank),
	}
	p.res.Units = append(p.res.Units, u)
	idx := len(p.res.Units) - 1
	p.stack = append(p.stack, open{idx: idx, rank: rank})
	p.pendingTitle = -1
	return idx
}

// path builds the citation path. Provisions are cited from the article down
// ("art.15/ust.1"), independent of the headings above them; headings form
// their own chain ("dzial.II/rozdzial.1").
func (p *parser) path(parent int, typ, number string, rank int) string {
	seg := typ + "." + number
	if parent < 0 {
		return seg
	}
	pu := p.res.Units[parent]
	parentIsHeading := headingRanks[pu.Type] != 0
	if rank == rankBasic && parentIsHeading {
		return seg
	}
	return pu.Path + "/" + seg
}

func (p *parser) continuation(line string) {
	if p.pendingTitle >= 0 {
		p.res.Units[p.pendingTitle].Heading = line
		p.pendingTitle = -1
		return
	}
	if len(p.stack) == 0 {
		p.preamble = append(p.preamble, line)
		return
	}
	p.appendText(p.stack[len(p.stack)-1].idx, line)
}

// appendText joins wrapped lines, undoing end-of-line hyphenation from PDFs
// ("przedsię-" + "biorca" → "przedsiębiorca").
func (p *parser) appendText(idx int, line string) {
	u := &p.res.Units[idx]
	switch {
	case u.Text == "":
		u.Text = line
	case strings.HasSuffix(u.Text, "-") && startsLower(line) && !strings.HasSuffix(u.Text, " -"):
		u.Text = strings.TrimSuffix(u.Text, "-") + line
	default:
		u.Text += " " + line
	}
}

func startsLower(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLower(r)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
