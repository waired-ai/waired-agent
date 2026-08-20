package installscripts

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// install.ps1 cannot hold its banner as literal text: `iwr … | iex` coerces
// the downloaded bytes through the client's ANSI code page, so the rows ship
// base64-encoded and are decoded at runtime by Utf8FromB64
// (TestInstallerPS1ScriptsArePureASCII guards that). The cost is that the
// banner's words are invisible to grep -- which is how "OpenCode" survived in
// the Windows banner for a full release after the integration was withdrawn
// (waired-agent#333/#355) and install.sh had already dropped it
// (waired-agent#798 (a)).
//
// These two tests decode the rows so the banner's text is reviewable and
// mirrored, the way every other install.sh/install.ps1 pair is
// (CLAUDE.md §Cross-OS parity).

var (
	ps1BannerRow = regexp.MustCompile(`@\((\d+),(\d+),(\d+),'([A-Za-z0-9+/=]+)'\)`)
	shBannerRow  = regexp.MustCompile(`_banner_row\s+(\d+)\s+(\d+)\s+(\d+)\s+"(.*)"`)
)

// decodedPS1Banner returns install.ps1's banner rows, in the order they are
// printed.
func decodedPS1Banner(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash("packaging/install/install.ps1")))
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	var out []string
	for _, m := range ps1BannerRow.FindAllStringSubmatch(string(b), -1) {
		raw, err := base64.StdEncoding.DecodeString(m[4])
		if err != nil {
			t.Fatalf("banner row %s,%s,%s is not valid base64: %v", m[1], m[2], m[3], err)
		}
		out = append(out, string(raw))
	}
	if len(out) == 0 {
		t.Fatal("no base64 banner rows found in install.ps1 -- the row syntax changed and this guard now checks nothing")
	}
	return out
}

// wordRows keeps the banner rows that carry words, dropping the ASCII-art and
// rule rows. The mirror this guards is the banner's *text*: the two files'
// figlet blocks and dashed rules already differ by a couple of characters in
// width, which is cosmetic, predates this guard and is not what let "OpenCode"
// survive. Narrowing here rather than "fixing" the art keeps the guard about
// the defect it exists for.
func wordRows(rows []string) []string {
	var out []string
	for _, r := range rows {
		if strings.ContainsAny(r, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			out = append(out, r)
		}
	}
	return out
}

// TestWindowsBannerNamesTheShippingIntegrations is the regression guard for
// waired-agent#798 (a). Written as a ban list rather than an equality check on
// the whole line: the point is that a *retired* name must not survive a
// withdrawal, and an equality check would have to be edited (and could be
// edited wrongly) every time the tagline is reworded.
func TestWindowsBannerNamesTheShippingIntegrations(t *testing.T) {
	joined := strings.Join(decodedPS1Banner(t), "\n")

	// Withdrawn integrations. OpenCode: waired-agent#333 removed it,
	// waired-agent#355 withdrew the compatibility shims.
	for _, retired := range []string{"OpenCode"} {
		if strings.Contains(joined, retired) {
			t.Errorf("install.ps1's banner still names the retired %q integration:\n%s", retired, joined)
		}
	}
	for _, shipping := range []string{"Claude Code", "OpenClaw"} {
		if !strings.Contains(joined, shipping) {
			t.Errorf("install.ps1's banner no longer names %q; if that is intended, update this guard", shipping)
		}
	}
}

// TestBannerRowsMirrorBetweenInstallers pins that the banner's WORD rows say
// the same thing in both installers. The mirroring rule (CLAUDE.md) is
// enforced structurally by mirror-guard.yml, which only sees that both files
// changed -- not that they still agree. This closes that half for the one
// surface where a drift is invisible to review, because one side is base64.
func TestBannerRowsMirrorBetweenInstallers(t *testing.T) {
	ps1 := wordRows(decodedPS1Banner(t))

	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash("packaging/install/install.sh")))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	matches := shBannerRow.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		t.Fatal("no _banner_row calls found in install.sh -- the row syntax changed and this guard now checks nothing")
	}
	var sh []string
	for _, m := range matches {
		// install.sh writes the row through printf, so a literal dollar is
		// escaped in the source. Undo that one escape before comparing.
		sh = append(sh, strings.ReplaceAll(m[4], `\$`, `$`))
	}
	sh = wordRows(sh)

	if len(sh) == 0 || len(ps1) == 0 {
		t.Fatalf("word rows: install.sh %d, install.ps1 %d -- nothing to compare", len(sh), len(ps1))
	}
	if len(sh) != len(ps1) {
		t.Fatalf("install.sh has %d banner word rows, install.ps1 has %d:\n  sh:  %q\n  ps1: %q",
			len(sh), len(ps1), sh, ps1)
	}
	for i := range sh {
		if sh[i] != ps1[i] {
			t.Errorf("banner word row %d differs between installers:\n  install.sh:  %q\n  install.ps1: %q",
				i, sh[i], ps1[i])
		}
	}
}
