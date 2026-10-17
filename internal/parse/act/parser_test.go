package act

import (
	"strings"
	"testing"
)

func lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

type want struct{ path, text string }

func check(t *testing.T, got Result, wants []want) {
	t.Helper()
	byPath := map[string]Unit{}
	for _, u := range got.Units {
		byPath[u.Path] = u
	}
	for _, w := range wants {
		u, ok := byPath[w.path]
		if !ok {
			var paths []string
			for _, u := range got.Units {
				paths = append(paths, u.Path)
			}
			t.Fatalf("missing unit %s; have %v", w.path, paths)
		}
		if w.text != "" && u.Text != w.text {
			t.Errorf("%s text = %q, want %q", w.path, u.Text, w.text)
		}
	}
}

func TestStatuteWithHeadingsUstPktLitTiret(t *testing.T) {
	r := Parse(lines(`
USTAWA
z dnia 15 lutego 1992 r.
o podatku dochodowym od osób prawnych
Rozdział 1
Przepisy ogólne
Art. 1. 1. Ustawa reguluje opodatkowanie podatkiem dochodowym dochodów osób prawnych.
2. Przepisy ustawy mają również zastosowanie do:
1) spółek kapitałowych w organizacji;
2) jednostek organizacyjnych, w tym:
a) spółek komandytowych,
b) spółek jawnych, jeżeli:
– wspólnikami nie są wyłącznie osoby fizyczne,
– spółka nie złożyła informacji.
Art. 1a. (uchylony)
Rozdział 2
Przedmiot opodatkowania
Art. 7. Przedmiotem opodatkowania jest dochód.
`))
	if !strings.Contains(r.Preamble, "o podatku dochodowym od osób prawnych") {
		t.Errorf("preamble = %q", r.Preamble)
	}
	check(t, r, []want{
		{"rozdzial.1", ""},
		{"art.1", ""},
		{"art.1/ust.1", "Ustawa reguluje opodatkowanie podatkiem dochodowym dochodów osób prawnych."},
		{"art.1/ust.2", "Przepisy ustawy mają również zastosowanie do:"},
		{"art.1/ust.2/pkt.1", "spółek kapitałowych w organizacji;"},
		{"art.1/ust.2/pkt.2/lit.a", "spółek komandytowych,"},
		{"art.1/ust.2/pkt.2/lit.b/tiret.1", "wspólnikami nie są wyłącznie osoby fizyczne,"},
		{"art.1/ust.2/pkt.2/lit.b/tiret.2", "spółka nie złożyła informacji."},
		{"art.1a", "(uchylony)"},
		{"art.7", "Przedmiotem opodatkowania jest dochód."},
	})
	for _, u := range r.Units {
		if u.Path == "rozdzial.1" && u.Heading != "Przepisy ogólne" {
			t.Errorf("heading = %q", u.Heading)
		}
		if u.Path == "art.7" && r.Units[u.Parent].Path != "rozdzial.2" {
			t.Errorf("art.7 parent = %s, want rozdzial.2", r.Units[u.Parent].Path)
		}
	}
}

func TestCodeWithParagraphs(t *testing.T) {
	r := Parse(lines(`
KSIĘGA PIERWSZA
CZĘŚĆ OGÓLNA
TYTUŁ I
Przepisy wstępne
Art. 1. Kodeks niniejszy reguluje stosunki cywilnoprawne między osobami fizycznymi i osobami prawnymi.
Art. 5. § 1. Nie można czynić ze swego prawa użytku, który by był sprzeczny ze społeczno-gospodarczym przeznaczeniem tego prawa.
§ 2. Przepis stosuje się odpowiednio.
Art. 415. Kto z winy swej wyrządził drugiemu szkodę, obowiązany jest do jej naprawienia.
`))
	check(t, r, []want{
		{"art.1", "Kodeks niniejszy reguluje stosunki cywilnoprawne między osobami fizycznymi i osobami prawnymi."},
		{"art.5/par.1", "Nie można czynić ze swego prawa użytku, który by był sprzeczny ze społeczno-gospodarczym przeznaczeniem tego prawa."},
		{"art.5/par.2", "Przepis stosuje się odpowiednio."},
		{"art.415", "Kto z winy swej wyrządził drugiemu szkodę, obowiązany jest do jej naprawienia."},
		{"ksiega.PIERWSZA/tytul.I", ""},
	})
	if r.Units[0].Heading != "CZĘŚĆ OGÓLNA" {
		t.Errorf("ksiega heading = %q", r.Units[0].Heading)
	}
}

func TestRegulationUsesParagraphAsBasicUnit(t *testing.T) {
	r := Parse(lines(`
ROZPORZĄDZENIE MINISTRA FINANSÓW
§ 1. 1. Rozporządzenie określa wzory deklaracji.
2. Wzory stanowią załącznik do rozporządzenia.
§ 2. Rozporządzenie wchodzi w życie z dniem 1 stycznia 2025 r.
`))
	check(t, r, []want{
		{"par.1/ust.1", "Rozporządzenie określa wzory deklaracji."},
		{"par.1/ust.2", "Wzory stanowią załącznik do rozporządzenia."},
		{"par.2", "Rozporządzenie wchodzi w życie z dniem 1 stycznia 2025 r."},
	})
}

func TestAmendingActQuotesAreNotParsed(t *testing.T) {
	r := Parse(lines(`
Art. 1. W ustawie z dnia 11 marca 2004 r. o podatku od towarów i usług wprowadza się następujące zmiany:
1) art. 5 otrzymuje brzmienie:
„Art. 5. 1. Opodatkowaniu podatkiem podlegają:
1) odpłatna dostawa towarów;
2. Czynności podlegają opodatkowaniu niezależnie od tego, czy zostały wykonane z zachowaniem warunków.”;
2) w art. 6 uchyla się pkt 2.
Art. 2. Ustawa wchodzi w życie po upływie 14 dni od dnia ogłoszenia.
`))
	var paths []string
	for _, u := range r.Units {
		paths = append(paths, u.Path)
	}
	wantPaths := []string{"art.1", "art.1/pkt.1", "art.1/pkt.2", "art.2"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
	if !strings.Contains(r.Units[1].Text, "„Art. 5. 1. Opodatkowaniu") {
		t.Errorf("quoted text should stay inside pkt 1: %q", r.Units[1].Text)
	}
}

func TestSuperscriptsAndHyphenation(t *testing.T) {
	r := Parse(lines(`
Art. 15a¹. Przepis dotyczy przedsię-
biorców oraz polsko- -niemieckich spółek.
Art. 4^1. Przepis szczególny.
`))
	check(t, r, []want{
		{"art.15a^1", "Przepis dotyczy przedsiębiorców oraz polsko- -niemieckich spółek."},
		{"art.4^1", "Przepis szczególny."},
	})
}

func TestUnrecognizedLinesAreKept(t *testing.T) {
	r := Parse(lines(`
Art. 1. Pierwsze zdanie
drugie zdanie po zawinięciu wiersza.
a) to nie jest litera bez punktu
`))
	check(t, r, []want{
		{"art.1", "Pierwsze zdanie drugie zdanie po zawinięciu wiersza. a) to nie jest litera bez punktu"},
	})
}
