package web

import "testing"

func TestPathLabel(t *testing.T) {
	cases := map[string]string{
		"art.15/ust.1/pkt.2/lit.a": "art. 15 ust. 1 pkt 2 lit. a",
		"art.5/par.2":              "art. 5 § 2",
		"art.4^1":                  "art. 4¹",
		"par.3":                    "§ 3",
	}
	for in, want := range cases {
		if got := PathLabel(in); got != want {
			t.Errorf("PathLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
