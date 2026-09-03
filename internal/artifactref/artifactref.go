// Package artifactref encodes/decodes a lightweight reference to one
// artifact.Service revision as a genai.Part - the shape that flows through
// plans, session events, and the gen_ai ledger instead of attachment bytes.
package artifactref

import (
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/genai"
)

// AppName is the ADK artifact.Service app name for all quack chat
// attachments. Mirrors orchestrator.AppName/store's chatAppName (each
// package keeps its own copy to avoid an import cycle).
const AppName = "quack"

// Scheme names a reference's FileData.FileURI - never a scheme a real
// FileData part carries (gs://, https://, ...), so it can't collide.
const Scheme = "quack-artifact"

// InlineMaxBytes bounds a single inline artifact write/read across the
// codebase (matches acp.readArtifactMaxBytes/orchestrator.loadArtifactMaxBytes).
const InlineMaxBytes = 256 * 1024

// Encode builds a reference part for one artifact revision. FileData (not
// InlineData, which exists to embed bytes, or Text, which could collide
// with real model-visible text) carries it.
func Encode(userID, sessionID, name string, revision int64, mimeType string) *genai.Part {
	u := url.URL{
		Scheme: Scheme,
		Path:   "/" + url.PathEscape(userID) + "/" + url.PathEscape(sessionID) + "/" + url.PathEscape(name),
	}
	q := url.Values{}
	q.Set("v", strconv.FormatInt(revision, 10))
	u.RawQuery = q.Encode()
	return &genai.Part{FileData: &genai.FileData{FileURI: u.String(), MIMEType: mimeType}}
}

// Decode extracts an artifact locator from p, or ok=false if p is nil or
// not one of ours (including a genuine FileData part under a different scheme).
func Decode(p *genai.Part) (userID, sessionID, name string, revision int64, ok bool) {
	if p == nil || p.FileData == nil || p.FileData.FileURI == "" {
		return "", "", "", 0, false
	}
	u, err := url.Parse(p.FileData.FileURI)
	if err != nil || u.Scheme != Scheme {
		return "", "", "", 0, false
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(segs) != 3 {
		return "", "", "", 0, false
	}
	uid, err1 := url.PathUnescape(segs[0])
	sid, err2 := url.PathUnescape(segs[1])
	nm, err3 := url.PathUnescape(segs[2])
	if err1 != nil || err2 != nil || err3 != nil || uid == "" || sid == "" || nm == "" {
		return "", "", "", 0, false
	}
	rev, err := strconv.ParseInt(u.Query().Get("v"), 10, 64)
	if err != nil {
		return "", "", "", 0, false
	}
	return uid, sid, nm, rev, true
}
