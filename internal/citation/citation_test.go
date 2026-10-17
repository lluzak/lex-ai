package citation

import (
	"strings"
	"testing"
)

func TestFind(t *testing.T) {
	cases := []struct {
		text  string
		paths []string
		hint  string
	}{
		{"zgodnie z art. 15 ust. 1 pkt 2 lit. a ustawy o CIT, koszty...", []string{"art.15/ust.1/pkt.2/lit.a"}, "ustawy o CIT"},
		{"na podstawie art. 5 § 2 k.c. sąd", []string{"art.5/par.2"}, "k.c"},
		{"art. 86 ust. 1 i 2 ustawy o podatku od towarów i usług (Dz. U. z 2024 r. poz. 361)",
			[]string{"art.86/ust.1", "art.86/ust.2"}, "ustawy o podatku od towarów i usług"},
		{"Art. 415 KC", []string{"art.415"}, "KC"},
		{"w myśl § 3 rozporządzenia Ministra Finansów", []string{"par.3"}, "rozporządzenia Ministra Finansów"},
		{"art. 4^1 pkt 3", []string{"art.4^1/pkt.3"}, ""},
	}
	for _, c := range cases {
		refs := Find(c.text)
		var paths []string
		for _, r := range refs {
			paths = append(paths, r.Path)
		}
		if strings.Join(paths, ",") != strings.Join(c.paths, ",") {
			t.Errorf("Find(%q) paths = %v, want %v", c.text, paths, c.paths)
			continue
		}
		if refs[0].ActHint != c.hint {
			t.Errorf("Find(%q) hint = %q, want %q", c.text, refs[0].ActHint, c.hint)
		}
	}
}

func TestResolveAct(t *testing.T) {
	aliases := map[string]string{
		"kc":                                  "DU/1964/93",
		"ustawy o podatku od towarów i usług": "DU/2004/535",
		"ustawy o cit":                        "DU/1992/86",
	}
	for hint, want := range map[string]string{
		"k.c":                                 "DU/1964/93",
		"KC":                                  "DU/1964/93",
		"ustawy o podatku od towarów i usług": "DU/2004/535",
		"ustawy o CIT, koszty":                "DU/1992/86",
	} {
		if got, ok := ResolveAct(hint, aliases); !ok || got != want {
			t.Errorf("ResolveAct(%q) = %q, %v; want %q", hint, got, ok, want)
		}
	}
	if _, ok := ResolveAct("ustawy o czymś innym", aliases); ok {
		t.Error("unknown act should not resolve")
	}
}
