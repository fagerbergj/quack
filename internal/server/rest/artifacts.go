package rest

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"

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
		revs[i] = schema.ArtifactRevisionInfo{
			Revision:  rv.Revision,
			MimeType:  rv.MimeType,
			Size:      rv.Size,
			TurnId:    strPtr(rv.TurnID),
			CreatedAt: &rv.CreatedAt,
		}
	}
	return schema.ArtifactSummary{Name: s.Name, Revisions: revs}
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
