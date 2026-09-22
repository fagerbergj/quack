package vetting

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// UnitCheck is the locate tier's verdict on one specific: where the cited
// evidence mentions it, or why it could not be looked for. It never means "supported".
type UnitCheck struct {
	Unit     Unit
	Specific Specific
	Citation string
	State    string // "located", "unlocated", "uncited", "no_stored_text"
	Window   string // evidence text around the match, empty unless located
}

// pageLoader is the one record-store call the resolver needs; a test can fake it.
type pageLoader interface {
	Latest(ctx context.Context, id string) ([]byte, int, bool, error)
}

// WebPageEvidence resolves a citation URL to the page text a fetch stored for it.
type WebPageEvidence struct{ Store pageLoader }

const webPageKind = "web_page"

// Resolve tries the URL as cited, then without fragment and trailing slash:
// the worker fetched one exact form and the citation is often a lighter one.
func (w WebPageEvidence) Resolve(ctx context.Context, citation string) (string, bool) {
	if w.Store == nil {
		return "", false
	}
	for _, cand := range urlVariants(citation) {
		id, err := recordstore.IdentityFor(webPageKind, "", cand)
		if err != nil {
			continue
		}
		if data, _, ok, err := w.Store.Latest(ctx, id); err == nil && ok && len(data) > 0 {
			return string(data), true
		}
	}
	return "", false
}

func urlVariants(raw string) []string {
	out := []string{raw}
	u, err := url.Parse(raw)
	if err != nil {
		return out
	}
	u.Fragment = ""
	bare := u.String()
	for _, v := range []string{bare, strings.TrimSuffix(bare, "/"), bare + "/"} {
		if v != raw && v != "" {
			out = append(out, v)
		}
	}
	return out
}

const locateWindow = 240

var thousandsRe = regexp.MustCompile(`(\d),(\d{3})`)

// LocateSpecific finds s in text after the same normalisation FindUnits applied
// (thousands separators dropped, case folded) and returns the surrounding window.
func LocateSpecific(text string, s Specific) (string, bool) {
	hay := strings.ToLower(text)
	needle := s.Norm
	if s.Kind == "number" || s.Kind == "percent" || s.Kind == "currency" {
		for thousandsRe.MatchString(hay) {
			hay = thousandsRe.ReplaceAllString(hay, "$1$2")
		}
	}
	if needle == "" {
		return "", false
	}
	i := indexSpecific(hay, needle, s.Kind)
	if i < 0 {
		return "", false
	}
	lo, hi := max(0, i-locateWindow), min(len(hay), i+len(needle)+locateWindow)
	return strings.TrimSpace(hay[lo:hi]), true
}

// indexSpecific: a figure must stand alone (12 must not match inside 2012).
func indexSpecific(hay, needle, kind string) int {
	from := 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return -1
		}
		i += from
		if kind != "number" && kind != "percent" && kind != "currency" {
			return i
		}
		if !partOfNumber(hay, i-1, -1) && !partOfNumber(hay, i+len(needle), 1) {
			return i
		}
		from = i + 1
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// partOfNumber: the byte at j continues a figure (a digit, or a decimal point with a digit past it).
func partOfNumber(hay string, j, dir int) bool {
	if j < 0 || j >= len(hay) {
		return false
	}
	if isDigit(hay[j]) {
		return true
	}
	k := j + dir
	return hay[j] == '.' && k >= 0 && k < len(hay) && isDigit(hay[k])
}

// CheckUnits runs the locate tier over every specific: resolve the unit's
// first citation and look for the specific in it. Units without specifics are skipped.
func CheckUnits(ctx context.Context, units []Unit, res WebPageEvidence) []UnitCheck {
	var out []UnitCheck
	pages := map[string]string{}
	for _, u := range units {
		for _, s := range u.Specifics {
			c := UnitCheck{Unit: u, Specific: s}
			if len(u.Citations) == 0 {
				c.State = "uncited"
				out = append(out, c)
				continue
			}
			c.Citation = u.Citations[0]
			text, ok := pages[c.Citation]
			if !ok {
				text, ok = res.Resolve(ctx, c.Citation)
				if ok {
					pages[c.Citation] = text
				}
			}
			switch {
			case !ok:
				c.State = "no_stored_text"
			default:
				c.Window, ok = LocateSpecific(text, s)
				c.State = map[bool]string{true: "located", false: "unlocated"}[ok]
			}
			out = append(out, c)
		}
	}
	return out
}
