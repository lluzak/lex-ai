package lookup

import "testing"

func TestParse(t *testing.T) {
	aliases := map[string]string{"kc": "DU/1964/93", "ustawy o vat": "DU/2004/535"}
	cases := map[string]Target{
		"Dz.U. 2024 poz. 1":           {ELI: "DU/2024/1"},
		"Dz. U. z 2024 r. poz. 361":   {ELI: "DU/2024/361"},
		"dz.u. 1964 nr 16 poz. 93":    {ELI: "DU/1964/93"},
		"M.P. 2023 poz. 5":            {ELI: "MP/2023/5"},
		"DU/2024/1":                   {ELI: "DU/2024/1"},
		"art. 415 k.c.":               {ELI: "DU/1964/93", Path: "art.415"},
		"art. 86 ust. 1 ustawy o VAT": {ELI: "DU/2004/535", Path: "art.86/ust.1"},
	}
	for q, want := range cases {
		got, ok := Parse(q, aliases)
		if !ok || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", q, got, ok, want)
		}
	}
	for _, q := range []string{"ulga na złe długi", "odpowiedzialność za szkodę z art. 415 k.c. w praktyce", "art. 15"} {
		if got, ok := Parse(q, aliases); ok {
			t.Errorf("Parse(%q) = %+v, want no exact target", q, got)
		}
	}
}
