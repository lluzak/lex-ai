package sejmeli

import (
	"encoding/json"
	"strings"
	"time"
)

// Field names follow the public ELI API of the Sejm (api.sejm.gov.pl/eli).
// They were written from the API documentation and must be confirmed against
// live responses in the source spike (docs/sources/sejm-eli.md). Unknown fields
// are ignored and the raw JSON is always kept in the blob store, so a field
// fix only needs a re-parse, never a re-fetch.

// ActList is the response of GET /acts/{publisher}/{year}.
type ActList struct {
	Count      int          `json:"count"`
	TotalCount int          `json:"totalCount"`
	Items      []ActSummary `json:"items"`
}

type ActSummary struct {
	ELI              string `json:"ELI"`     // "DU/2024/1"
	Address          string `json:"address"` // "WDU20240000001"
	Publisher        string `json:"publisher"`
	Year             int    `json:"year"`
	Pos              int    `json:"pos"`
	Type             string `json:"type"` // "Ustawa", "Rozporządzenie", ...
	Title            string `json:"title"`
	DisplayAddress   string `json:"displayAddress"` // "Dz.U. 2024 poz. 1"
	Status           string `json:"status"`
	Promulgation     string `json:"promulgation"`
	AnnouncementDate string `json:"announcementDate"`
	ChangeDate       string `json:"changeDate"`
	TextHTML         bool   `json:"textHTML"`
	TextPDF          bool   `json:"textPDF"`
}

// Act is the response of GET /acts/{publisher}/{year}/{pos}.
type Act struct {
	ActSummary
	InForce        string                 `json:"inForce"`
	EntryIntoForce string                 `json:"entryIntoForce"`
	ValidFrom      string                 `json:"validFrom"`
	Keywords       []string               `json:"keywords"`
	ReleasedBy     []string               `json:"releasedBy"`
	References     map[string][]Reference `json:"references"`
}

// Reference is one entry of an act's "references" map. The map key is the
// relation label used by the source, e.g. "Akty zmienione", "Tekst jednolity",
// "Podstawa prawna".
type Reference struct {
	ID   string `json:"id"` // "WDU19640160093" style address
	Art  string `json:"art"`
	Date string `json:"date"`
}

// ParseAct decodes the act details JSON.
func ParseAct(raw []byte) (Act, error) {
	var a Act
	err := json.Unmarshal(raw, &a)
	return a, err
}

// ExternalID is the stable id we use for an act: its ELI path.
func (s ActSummary) ExternalID() string {
	if s.ELI != "" {
		return s.ELI
	}
	return ELI(s.Publisher, s.Year, s.Pos)
}

// AddressToELI converts an ISAP address such as "WDU19640160093" to "DU/1964/93".
// The address is W + publisher (DU/MP) + 4-digit year + 7-digit position
// (older addresses embed the journal number in the first digits).
func AddressToELI(addr string) (string, bool) {
	if len(addr) != 14 || addr[0] != 'W' {
		return "", false
	}
	pub := addr[1:3]
	if pub != "DU" && pub != "MP" {
		return "", false
	}
	year := addr[3:7]
	// Last 4 digits are the position; preceding 3 are the journal number (Nr).
	pos := strings.TrimLeft(addr[10:], "0")
	if pos == "" {
		return "", false
	}
	return pub + "/" + year + "/" + pos, true
}

// Date parses the date formats used by the API ("2006-01-02" or RFC 3339).
func Date(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
