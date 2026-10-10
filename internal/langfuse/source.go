package langfuse

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

// Source adapts *Client to artifactsrc.Source; an auth failure is wrapped so
// the resolver's log names the store to check.
type Source struct {
	Client   *Client
	StoreKey string
}

// Get resolves name through the pinned label. Source stays blank so the Resolver
// stamps the stores: entry name that answered.
func (s *Source) Get(ctx context.Context, name string) (artifactsrc.Artifact, bool, error) {
	p, found, err := s.Client.Resolve(ctx, name)
	if err != nil {
		if IsAuthError(err) {
			return artifactsrc.Artifact{}, false, fmt.Errorf("%w: check stores.%s credentials", err, s.StoreKey)
		}
		return artifactsrc.Artifact{}, false, err
	}
	if !found {
		return artifactsrc.Artifact{}, false, nil
	}
	return artifactsrc.Artifact{
		Body:      p.Body,
		Config:    p.Config,
		VersionID: strconv.Itoa(p.Version),
	}, true, nil
}

// Seed pushes the shipped version of name to Langfuse per #1418's seeding
// rules and logs the outcome - "unchanged" is silent, an operator edit only warns.
func (s *Source) Seed(ctx context.Context, name string, static artifactsrc.Artifact) error {
	action, err := s.Client.Seed(ctx, name, static.Body, static.VersionID)
	if err != nil {
		return err
	}
	switch action {
	case Created, Updated:
		slog.Info("langfuse prompt seeded", "component", "artifacts", "artifact", name, "action", string(action))
	case OperatorEdited:
		slog.Warn("shipped "+name+" changed; Langfuse has operator edits, diff in the Langfuse UI",
			"component", "artifacts", "artifact", name)
	}
	return nil
}
