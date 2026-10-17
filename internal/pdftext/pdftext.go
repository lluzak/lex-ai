// Package pdftext extracts text from PDFs with poppler's pdftotext. Go has no
// mature PDF library, so the CLI is shipped in the container image instead.
package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var ErrUnavailable = errors.New("pdftotext is not installed")

// Extract returns the text of a PDF. Scanned PDFs without a text layer return
// little or no text; those need OCR (a later pipeline stage).
func Extract(ctx context.Context, pdf []byte) (string, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", ErrUnavailable
	}
	f, err := os.CreateTemp("", "lexai-*.pdf")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(pdf); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-enc", "UTF-8", "-nopgbrk", f.Name(), "-")
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// Page furniture printed on every page of Dziennik Ustaw / Monitor Polski PDFs.
var furniture = []*regexp.Regexp{
	regexp.MustCompile(`^(Dziennik Ustaw|Monitor Polski)\s+[–-]\s+\d+\s+[–-]\s+Poz\.\s*\d+\s*$`),
	regexp.MustCompile(`^\d{1,4}$`), // lone page numbers
}

// StripFurniture removes running headers and page numbers from extracted lines.
func StripFurniture(lines []string) []string {
	out := lines[:0:0]
	for _, l := range lines {
		skip := false
		for _, re := range furniture {
			if re.MatchString(l) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, l)
		}
	}
	return out
}
