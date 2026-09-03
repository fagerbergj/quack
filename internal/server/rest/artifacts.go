package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/aymanbagabas/go-udiff"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// inlineArtifactMimeTypes is the ONLY set of MIME types GetChatArtifact
// renders inline. Everything else - including image/svg+xml, which can
// carry a <script> - downloads as an attachment; same-origin stored-XSS via
// an SVG "image" is the trap this allowlist exists to close.
var inlineArtifactMimeTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ListChatArtifacts lists every artifact name visible to the chat, each
// with its full revision history. Unpaginated - bounded per chat.
func (h *Handler) ListChatArtifacts(w http.ResponseWriter, r *http.Request, chatID schema.ChatID) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	if h.artifacts == nil {
		writeJSON(w, http.StatusOK, schema.ArtifactList{Data: []schema.ArtifactSummary{}})
		return
	}
	userID := h.sessionUser(r.Context(), chatID)
	summaries, err := h.artifacts.ListForSession(r.Context(), artifactref.AppName, userID, chatID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := schema.ArtifactList{Data: make([]schema.ArtifactSummary, len(summaries))}
	for i, s := range summaries {
		out.Data[i] = toArtifactSummary(s)
	}
	writeJSON(w, http.StatusOK, out)
}

func toArtifactSummary(s store.ArtifactSummary) schema.ArtifactSummary {
	revs := make([]schema.ArtifactRevisionInfo, len(s.Revisions))
	for i, rv := range s.Revisions {
		revs[i] = toArtifactRevisionInfo(rv)
	}
	out := schema.ArtifactSummary{Name: s.Name, Revisions: revs}
	// s.Revisions is ascending (store.ListForSession's contract), so the last
	// entry carries the latest revision's kind/class/lineage.
	if n := len(revs); n > 0 {
		last := revs[n-1]
		out.Kind = last.Kind
		if last.Class != nil {
			class := schema.ArtifactSummaryClass(*last.Class)
			out.Class = &class
		}
		out.LatestRevision = &s.Revisions[n-1].Revision
		out.Lineage = last.Lineage
	}
	return out
}

func toArtifactRevisionInfo(rv store.ArtifactRevision) schema.ArtifactRevisionInfo {
	info := schema.ArtifactRevisionInfo{
		Revision:  rv.Revision,
		MimeType:  rv.MimeType,
		Size:      rv.Size,
		TurnId:    strPtr(rv.TurnID),
		CreatedAt: &rv.CreatedAt,
	}
	if rv.Kind != "" {
		info.Kind = strPtr(rv.Kind)
	}
	if rv.Class != "" {
		class := schema.ArtifactRevisionInfoClass(rv.Class)
		info.Class = &class
	}
	if rv.LineageJSON != "" {
		var l schema.ArtifactLineage
		if err := json.Unmarshal([]byte(rv.LineageJSON), &l); err == nil {
			info.Lineage = &l
		}
	}
	return info
}

// revisionsForArtifact fetches one artifact name's revisions - the store's
// WHERE name = ? seam (store.TurnAwareService.RevisionsForName) when the
// backend supports it, falling back to filtering ListForSession's full-chat
// listing only for a backend that doesn't (adversarial review follow-up on
// #1094: the original version always paid for the full-chat scan).
func (h *Handler) revisionsForArtifact(r *http.Request, chatID, name string) ([]store.ArtifactRevision, bool, error) {
	userID := h.sessionUser(r.Context(), chatID)
	revs, supported, err := h.artifacts.RevisionsForName(r.Context(), artifactref.AppName, userID, chatID, name)
	if err != nil {
		return nil, false, err
	}
	if supported {
		return revs, len(revs) > 0, nil
	}
	summaries, err := h.artifacts.ListForSession(r.Context(), artifactref.AppName, userID, chatID)
	if err != nil {
		return nil, false, err
	}
	for _, s := range summaries {
		if s.Name == name {
			return s.Revisions, true, nil
		}
	}
	return nil, false, nil
}

// ListArtifactRevisions lists one artifact id's revisions, newest first,
// each with lineage - the revision picker's data source (#1094).
func (h *Handler) ListArtifactRevisions(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, artifactName schema.ArtifactName) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	if h.artifacts == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	storeRevs, ok, err := h.revisionsForArtifact(r, chatID, artifactName)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	revs := make([]schema.ArtifactRevisionInfo, len(storeRevs))
	for i, rv := range storeRevs {
		revs[i] = toArtifactRevisionInfo(rv)
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].Revision > revs[j].Revision })
	writeJSON(w, http.StatusOK, schema.ArtifactRevisionList{Data: revs})
}

// diffableMimeTypes: byte-level diffs of binary blobs (images, PDFs) render
// as noise, not a review aid - DiffArtifactRevisions 415s anything outside
// this allowlist instead of pretending a diff exists.
func diffable(mimeType string) bool {
	return mimeType == "application/json" || strings.HasPrefix(mimeType, "text/")
}

// diffRevisionMaxBytes mirrors internal/acp/memorymcp.go's read_artifact
// bound (256KB) - the same "don't let one huge revision flood the response"
// ceiling, duplicated rather than imported since acp is another agent's
// package for this change and the constant isn't exported.
const diffRevisionMaxBytes = 256 * 1024

// DiffArtifactRevisions returns a unified diff between two revisions of one
// artifact, text/structured only (415 for a binary blob).
func (h *Handler) DiffArtifactRevisions(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, artifactName schema.ArtifactName, params schema.DiffArtifactRevisionsParams) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	if h.artifacts == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	userID := h.sessionUser(r.Context(), chatID)
	load := func(version int64) ([]byte, string, error) {
		resp, err := h.artifacts.Load(r.Context(), &artifact.LoadRequest{
			AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: artifactName, Version: version,
		})
		if err != nil {
			return nil, "", err
		}
		if resp.Part == nil || resp.Part.InlineData == nil {
			return nil, "", fs.ErrNotExist
		}
		return resp.Part.InlineData.Data, resp.Part.InlineData.MIMEType, nil
	}
	fromData, fromMime, err := load(int64(params.From))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			errMsg(w, http.StatusNotFound, "not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	toData, toMime, err := load(int64(params.To))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			errMsg(w, http.StatusNotFound, "not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if !diffable(fromMime) || !diffable(toMime) {
		errMsg(w, http.StatusUnsupportedMediaType, "artifact is a binary blob; diffing is not supported")
		return
	}
	if len(fromData) > diffRevisionMaxBytes || len(toData) > diffRevisionMaxBytes {
		errMsg(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"revision exceeds the %d byte diff limit; fetch it directly via GET .../artifacts/%s instead", diffRevisionMaxBytes, artifactName))
		return
	}
	fromLabel := artifactName + "@" + strconv.Itoa(params.From)
	toLabel := artifactName + "@" + strconv.Itoa(params.To)
	out := udiff.Unified(fromLabel, toLabel, string(fromData), string(toData))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

// GetChatArtifact streams one artifact revision's bytes - latest by
// default, or ?revision=n. Content-Disposition defaults to attachment;
// only inlineArtifactMimeTypes renders inline.
func (h *Handler) GetChatArtifact(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, artifactName schema.ArtifactName, params schema.GetChatArtifactParams) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	if h.artifacts == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	userID := h.sessionUser(r.Context(), chatID)
	var version int64
	if params.Revision != nil {
		version = int64(*params.Revision)
	}
	resp, err := h.artifacts.Load(r.Context(), &artifact.LoadRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: artifactName, Version: version,
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			errMsg(w, http.StatusNotFound, "not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if resp.Part == nil || resp.Part.InlineData == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	data, mimeType := resp.Part.InlineData.Data, resp.Part.InlineData.MIMEType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	disposition := "attachment"
	if inlineArtifactMimeTypes[mimeType] {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", mimeType)
	// artifactName is caller-supplied so never reaches the header verbatim.
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": artifactName}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
