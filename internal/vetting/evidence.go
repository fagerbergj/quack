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
	State    string  // "located", "unlocated", "uncited", "no_stored_text"
	Window   string  // evidence around the match; for an unlocated figure, around the claim's key terms (second look)
	page     string  // the cited page's text, for the second look's wider window
	snippet  bool    // the evidence is a search snippet: it can back a specific, never contradict one
	Verdict  Verdict // the verify tier's answer, zero until it runs
}

// PageLoader is the one record-store call the resolver needs: recordstore.Client
// satisfies it, and a REST adapter does for replay against a server.
type PageLoader interface {
	Latest(ctx context.Context, id string) ([]byte, int, bool, error)
}

// WebPageEvidence resolves a citation URL to the page text a fetch stored for it,
// else to the search snippet the worker saw for it (owner: quoting a snippet is fine).
type WebPageEvidence struct {
	Store    PageLoader
	Snippets map[string]string // search result url -> snippet, from the round's activity
}

const webPageKind = "web_page"

// Resolve tries the URL as cited, then without fragment and trailing slash:
// the worker fetched one exact form and the citation is often a lighter one.
func (w WebPageEvidence) Resolve(ctx context.Context, citation string) (string, bool) {
	r := w.resolve(ctx, citation)
	return r.text, r.ok
}

type resolved struct {
	text        string
	snippet, ok bool
}

func (w WebPageEvidence) resolve(ctx context.Context, citation string) resolved {
	for _, cand := range urlVariants(citation) {
		if w.Store == nil {
			break
		}
		id, err := recordstore.IdentityFor(webPageKind, "", cand)
		if err != nil {
			continue
		}
		if data, _, ok, err := w.Store.Latest(ctx, id); err == nil && ok && len(data) > 0 {
			return resolved{text: string(data), ok: true}
		}
	}
	for _, cand := range urlVariants(citation) {
		if s := w.Snippets[cand]; s != "" {
			return resolved{text: s, snippet: true, ok: true}
		}
	}
	return resolved{}
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

var numberWords = map[string]string{"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13", "fourteen": "14", "fifteen": "15", "sixteen": "16", "seventeen": "17", "eighteen": "18", "nineteen": "19", "twenty": "20", "thirty": "30", "forty": "40", "fifty": "50", "sixty": "60", "seventy": "70", "eighty": "80", "ninety": "90", "hundred": "100", "thousand": "1000"}

var wordRe = regexp.MustCompile(`[a-z]+`)

// digitsForWords rewrites spelled-out numbers so "three of nine" carries the 9 a figure check looks for.
func digitsForWords(text string) string {
	return wordRe.ReplaceAllStringFunc(strings.ToLower(text), func(w string) string {
		if d, ok := numberWords[w]; ok {
			return d
		}
		return w
	})
}

// LocateSpecific finds s in text after the same normalisation FindUnits applied
// (thousands separators dropped, case folded) and returns the surrounding window.
func LocateSpecific(text string, s Specific) (string, bool) {
	return locateSpecificIn(text, s, locateWindow)
}

func locateSpecificIn(text string, s Specific, width int) (string, bool) {
	hay := strings.ToLower(text)
	needle := s.Norm
	if s.Kind == "number" || s.Kind == "percent" || s.Kind == "currency" {
		hay = digitsForWords(hay)
		for thousandsRe.MatchString(hay) {
			hay = thousandsRe.ReplaceAllString(hay, "$1$2")
		}
	}
	if needle == "" {
		return "", false
	}
	if s.Kind == "quote" {
		return locateQuote(hay, needle, width)
	}
	i := -1
	for _, n := range needleForms(s) {
		if i = indexSpecific(hay, n, s.Kind); i >= 0 {
			needle = n
			break
		}
	}
	if i < 0 {
		return "", false
	}
	lo, hi := max(0, i-width), min(len(hay), i+len(needle)+width)
	return strings.TrimSpace(hay[lo:hi]), true
}

var punctVariants = strings.NewReplacer("\u2019", "'", "\u2018", "'", "\u201c", "\"", "\u201d", "\"", "\u2013", "-", "\u2014", "-", "\u00a0", " ")

// locateQuote matches a quoted string segment by segment: an elided quote
// ("first part ... last part") holds when every segment appears in order, and
// curly punctuation or emphasis marks on either side do not break it.
func locateQuote(hay, quote string, width int) (string, bool) {
	hay = normalizeSpace(punctVariants.Replace(markupRe.ReplaceAllString(hay, "")))
	quote = normalizeSpace(punctVariants.Replace(markupRe.ReplaceAllString(quote, "")))
	var segs []string
	for _, seg := range strings.Split(strings.ReplaceAll(quote, "\u2026", "..."), "...") {
		if seg = strings.TrimSpace(seg); len(seg) >= 12 {
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		segs = []string{quote}
	}
	at, first := 0, -1
	for _, seg := range segs {
		i := strings.Index(hay[at:], seg)
		if i < 0 {
			return "", false
		}
		if first < 0 {
			first = at + i
		}
		at += i + len(seg)
	}
	lo, hi := max(0, first-width), min(len(hay), at+width)
	return strings.TrimSpace(hay[lo:hi]), true
}

var spaceRe = regexp.MustCompile(`\s+`)

func normalizeSpace(s string) string { return spaceRe.ReplaceAllString(s, " ") }

// needleForms: a date is looked for in every rendering a page might use
// (2026-09-15, sep 15, 2026, september 15, 2026, 15 sep 2026); other kinds as-is.
func needleForms(s Specific) []string {
	if s.Kind != "date" {
		return []string{s.Norm}
	}
	parts := strings.Split(s.Norm, "-")
	if len(parts) == 2 { // month-day without a year
		parts = append([]string{""}, parts...)
	}
	if len(parts) != 3 {
		return []string{s.Norm}
	}
	year, mon, day := parts[0], parts[1], strings.TrimPrefix(parts[2], "0")
	var name string
	for k, v := range monthNum {
		if v == mon {
			name = k
		}
	}
	long := map[string]string{"jan": "january", "feb": "february", "mar": "march", "apr": "april", "may": "may", "jun": "june", "jul": "july", "aug": "august", "sep": "september", "oct": "october", "nov": "november", "dec": "december"}[name]
	forms := []string{s.Norm, name + " " + day + ", " + year, long + " " + day + ", " + year, day + " " + name + " " + year, day + " " + long + " " + year, name + " " + day, long + " " + day}
	var out []string
	for _, f := range forms {
		if f = strings.TrimSpace(strings.TrimSuffix(f, ", ")); f != "" {
			out = append(out, f)
		}
	}
	return out
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

// locateAcross tries every citation the unit carries: a sentence with two
// links may back its figure with the second. Stored text on any page wins over none.
func locateAcross(ctx context.Context, c UnitCheck, citations []string, res WebPageEvidence, pages map[string]resolved) UnitCheck {
	c.State = "no_stored_text"
	for _, cit := range citations {
		r, cached := pages[cit]
		if !cached {
			r = res.resolve(ctx, cit)
			pages[cit] = r
		}
		if !r.ok {
			continue
		}
		text := r.text
		if c.State == "no_stored_text" {
			c.State, c.Citation = "unlocated", cit
		}
		if w, found := LocateSpecific(text, c.Specific); found {
			c.State, c.Citation, c.Window, c.page, c.snippet = "located", cit, w, text, r.snippet
			return c
		}
		if c.Window == "" {
			if w := keyTermWindow(text, withoutLinks(c.Unit.Text), locateWindow); w != "" { // second look: where the claim's own terms sit
				c.Window, c.Citation, c.page, c.snippet = w, cit, text, r.snippet // the row is reported under the page its window came from
			}
		}
	}
	return c
}

var termRe = regexp.MustCompile(`[a-z]{6,}`)

// wideWindow is the second look's evidence: the same place on the page, cut three times wider.
func (c UnitCheck) wideWindow() string {
	w, ok := locateSpecificIn(c.page, c.Specific, 3*locateWindow)
	if !ok {
		w = keyTermWindow(c.page, withoutLinks(c.Unit.Text), 3*locateWindow)
	}
	if w == "" {
		return c.Window
	}
	return w
}

// keyTermWindow cuts the page around the first of the unit's longer words that
// appears in it, so a figure the page states differently still gets read.
func keyTermWindow(page, unit string, width int) string {
	hay := strings.ToLower(page)
	for _, term := range termRe.FindAllString(strings.ToLower(unit), -1) {
		if i := strings.Index(hay, term); i >= 0 {
			lo, hi := max(0, i-width), min(len(hay), i+len(term)+width)
			return strings.TrimSpace(hay[lo:hi])
		}
	}
	return ""
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// partOfNumber: the byte at j continues a figure (a digit, or a decimal point with a digit past it).
func partOfNumber(hay string, j, dir int) bool {
	if j < 0 || j >= len(hay) {
		return false
	}
	if isDigit(hay[j]) || (dir < 0 && hay[j] == '-' && (j == 0 || hay[j-1] == ' ' || hay[j-1] == '\n' || hay[j-1] == '\t' || hay[j-1] == '(')) {
		return true // a sign belongs to the figure: 12 must not match inside -12
	}
	k := j + dir
	return hay[j] == '.' && k >= 0 && k < len(hay) && isDigit(hay[k])
}

// CheckUnits runs the locate tier over every specific: resolve the unit's
// first citation and look for the specific in it. Units without specifics are skipped.
func CheckUnits(ctx context.Context, units []Unit, res WebPageEvidence) []UnitCheck {
	var out []UnitCheck
	pages := map[string]resolved{}
	for _, u := range units {
		for _, s := range u.Specifics {
			c := UnitCheck{Unit: u, Specific: s}
			if len(u.Citations) == 0 {
				c.State = "uncited"
				out = append(out, c)
				continue
			}
			out = append(out, locateAcross(ctx, c, u.Citations, res, pages))
		}
	}
	return out
}
