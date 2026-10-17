package sejmeli

import "testing"

func TestAddressToELI(t *testing.T) {
	cases := map[string]string{
		"WDU19640160093": "DU/1964/93",
		"WDU20240000001": "DU/2024/1",
		"WMP20231234567": "MP/2023/4567",
	}
	for addr, want := range cases {
		if got, ok := AddressToELI(addr); !ok || got != want {
			t.Errorf("AddressToELI(%q) = %q, %v; want %q", addr, got, ok, want)
		}
	}
	for _, bad := range []string{"", "WDU2024", "XDU20240000001", "WXX20240000001", "WDU20240000000"} {
		if got, ok := AddressToELI(bad); ok {
			t.Errorf("AddressToELI(%q) = %q, want failure", bad, got)
		}
	}
}

func TestDate(t *testing.T) {
	for _, s := range []string{"2024-01-12", "2024-02-01T10:00:00", "2024-02-01T10:00:00Z"} {
		if _, ok := Date(s); !ok {
			t.Errorf("Date(%q) failed", s)
		}
	}
	if _, ok := Date("12.01.2024"); ok {
		t.Error("unexpected parse of Polish date format")
	}
}
