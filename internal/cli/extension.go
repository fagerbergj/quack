package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// RunExtensionRestart is `quack extension restart <name>`: one out-of-process extension's child restarts on its
// re-resolved binary (exec: release re-reads the plugin pin); the server and other extensions keep running.
func RunExtensionRestart(ctx context.Context, out io.Writer, server, name string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodPost, "/api/v1/extensions/"+url.PathEscape(name)+"/restart"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "restarted extension %s\n", name)
	return err
}
