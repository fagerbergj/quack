// Package recordstore is a typed-JSON record store over the ADK artifact.Service.
// It is record-type-agnostic: a record is identified by a name and holds one
// JSON document per auto-versioned revision under a fixed (app, user, session) key.
package recordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
)

// isNotFound reports whether err is the artifact.Service "no such
// artifact/version" sentinel, which every backend wraps around fs.ErrNotExist.
func isNotFound(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// Client scopes record reads/writes to one (appName, userID, sessionID).
type Client struct {
	svc                        artifact.Service
	appName, userID, sessionID string
}

// New scopes a client to one session over svc (the artifact.Service the
// concrete store wraps, e.g. *store.TurnAwareService).
func New(svc artifact.Service, appName, userID, sessionID string) *Client {
	return &Client{svc: svc, appName: appName, userID: userID, sessionID: sessionID}
}

// SaveJSON stores doc as a new revision of name (auto-revision, mime
// application/json) and returns the stored revision.
func (c *Client) SaveJSON(ctx context.Context, name string, doc any) (int, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return 0, fmt.Errorf("recordstore: marshal %s: %w", name, err)
	}
	resp, err := c.svc.Save(ctx, &artifact.SaveRequest{
		AppName:   c.appName,
		UserID:    c.userID,
		SessionID: c.sessionID,
		FileName:  name,
		Part:      &genai.Part{InlineData: &genai.Blob{Data: raw, MIMEType: "application/json"}},
	})
	if err != nil {
		return 0, fmt.Errorf("recordstore: save %s: %w", name, err)
	}
	return int(resp.Version), nil
}

// SaveJSONAsync is the fire-and-forget form: it saves in a goroutine with its
// own timeout and logs a Warn on error. Fail-open, same as commitMemoryOnPass.
func (c *Client) SaveJSONAsync(ctx context.Context, name string, doc any) {
	go func() {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := c.SaveJSON(saveCtx, name, doc); err != nil {
			slog.Warn("recordstore: async save failed", "component", "recordstore", "name", name, "err", err)
		}
	}()
}

// Latest returns the newest revision of name as raw JSON plus its revision.
// ok is false when no revision exists.
func (c *Client) Latest(ctx context.Context, name string) ([]byte, int, bool, error) {
	resp, err := c.svc.Load(ctx, &artifact.LoadRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: name,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("recordstore: load %s: %w", name, err)
	}
	if resp == nil || resp.Part == nil || resp.Part.InlineData == nil {
		return nil, 0, false, nil
	}
	vresp, err := c.svc.Versions(ctx, &artifact.VersionsRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: name,
	})
	rev := 0
	if err == nil && vresp != nil {
		for _, v := range vresp.Versions {
			if int(v) > rev {
				rev = int(v)
			}
		}
	}
	return resp.Part.InlineData.Data, rev, true, nil
}

// LoadVersion returns a specific revision of name as raw JSON, nil if absent.
func (c *Client) LoadVersion(ctx context.Context, name string, version int) ([]byte, error) {
	resp, err := c.svc.Load(ctx, &artifact.LoadRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: name, Version: int64(version),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("recordstore: load %s@%d: %w", name, version, err)
	}
	if resp == nil || resp.Part == nil || resp.Part.InlineData == nil {
		return nil, nil
	}
	return resp.Part.InlineData.Data, nil
}

// KeepLastRevisions deletes every revision of name older than the last keep.
// Built on Versions + per-old-revision Delete; log-and-continue, no transaction.
func (c *Client) KeepLastRevisions(ctx context.Context, name string, keep int) error {
	vresp, err := c.svc.Versions(ctx, &artifact.VersionsRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: name,
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("recordstore: versions %s: %w", name, err)
	}
	if vresp == nil || len(vresp.Versions) <= keep {
		return nil
	}
	versions := append([]int64(nil), vresp.Versions...)
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	toDrop := versions[:len(versions)-keep]
	for _, v := range toDrop {
		if err := c.svc.Delete(ctx, &artifact.DeleteRequest{
			AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: name, Version: v,
		}); err != nil {
			slog.Warn("recordstore: retention delete failed", "component", "recordstore", "name", name, "version", v, "err", err)
		}
	}
	return nil
}

// DeleteAll deletes every revision of name (Delete Version 0 deletes all).
func (c *Client) DeleteAll(ctx context.Context, name string) error {
	if err := c.svc.Delete(ctx, &artifact.DeleteRequest{
		AppName: c.appName, UserID: c.userID, SessionID: c.sessionID, FileName: name,
	}); err != nil {
		return fmt.Errorf("recordstore: delete %s: %w", name, err)
	}
	return nil
}

// Status marks a key's relation to the stored previous revision.
type Status string

const (
	Added     Status = "added"
	Changed   Status = "changed"
	Unchanged Status = "unchanged"
)

// Diff returns one status per top-level key of candidate, compared against
// the stored latest revision of name (or version, if > 0).
func (c *Client) Diff(ctx context.Context, name string, version int, candidate any) (map[string]Status, error) {
	candRaw, err := json.Marshal(candidate)
	if err != nil {
		return nil, fmt.Errorf("recordstore: marshal candidate for diff %s: %w", name, err)
	}
	var candMap map[string]json.RawMessage
	if err := json.Unmarshal(candRaw, &candMap); err != nil {
		return nil, fmt.Errorf("recordstore: candidate for diff %s is not a JSON object: %w", name, err)
	}

	var prevRaw []byte
	var ok bool
	if version > 0 {
		prevRaw, err = c.LoadVersion(ctx, name, version)
		ok = prevRaw != nil
	} else {
		prevRaw, _, ok, err = c.Latest(ctx, name)
	}
	if err != nil {
		return nil, err
	}
	prevMap := map[string]json.RawMessage{}
	if ok {
		if err := json.Unmarshal(prevRaw, &prevMap); err != nil {
			return nil, fmt.Errorf("recordstore: prior revision of %s is not a JSON object: %w", name, err)
		}
	}

	out := make(map[string]Status, len(candMap))
	for k, v := range candMap {
		prev, existed := prevMap[k]
		switch {
		case !existed:
			out[k] = Added
		case bytes.Equal(prev, v):
			out[k] = Unchanged
		default:
			out[k] = Changed
		}
	}
	return out, nil
}
