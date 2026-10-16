package android

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"strconv"
	"strings"
)

// adbClient speaks the adb host protocol to an adb server, so the quack image needs no adb binary.
type adbClient struct{ addr, serial string }

// maxOutput bounds one command's output; a full-resolution screencap PNG is a few MB.
const maxOutput = 32 << 20

// exec runs cmd through the device's sh via the exec: service, which returns raw, binary-safe stdout.
func (c *adbClient) exec(ctx context.Context, cmd string) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("adb: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return nil, fmt.Errorf("adb: %w", err)
		}
	}
	transport := "host:transport-any"
	if c.serial != "" {
		transport = "host:transport:" + c.serial
	}
	if err := request(conn, transport); err != nil {
		return nil, err
	}
	if err := request(conn, "exec:"+cmd); err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(conn, maxOutput))
	if err != nil {
		return nil, fmt.Errorf("adb: read %s: %w", service(cmd), err)
	}
	return out, nil
}

// request sends one length-prefixed service request and reads its OKAY or FAIL reply.
func request(rw io.ReadWriter, payload string) error {
	name := service(payload)
	if len(payload) > 0xffff {
		return fmt.Errorf("adb: %s: request too long", name)
	}
	if _, err := fmt.Fprintf(rw, "%04x%s", len(payload), payload); err != nil {
		return fmt.Errorf("adb: %s: %w", name, err)
	}
	status := make([]byte, 4)
	if _, err := io.ReadFull(rw, status); err != nil {
		return fmt.Errorf("adb: %s: %w", name, err)
	}
	switch string(status) {
	case "OKAY":
		return nil
	case "FAIL":
		n := make([]byte, 4)
		if _, err := io.ReadFull(rw, n); err != nil {
			return fmt.Errorf("adb: %s failed", name)
		}
		size, _ := strconv.ParseUint(string(n), 16, 16)
		msg := make([]byte, size)
		_, _ = io.ReadFull(rw, msg)
		return fmt.Errorf("adb: %s: %s", name, msg)
	}
	return fmt.Errorf("adb: %s: unexpected reply %q", name, status)
}

// service names a request in errors without its arguments, which may be text the agent typed.
func service(payload string) string {
	name, _, _ := strings.Cut(payload, " ")
	return name
}

// element is one interactive node: its label and the center of its bounds, where a tap lands.
type element struct {
	label string
	x, y  int
}

// node is a uiautomator dump node; encoding/xml parses its "true"/"false" attributes as bools.
type node struct {
	Class         string `xml:"class,attr"`
	Text          string `xml:"text,attr"`
	Desc          string `xml:"content-desc,attr"`
	ID            string `xml:"resource-id,attr"`
	Bounds        string `xml:"bounds,attr"`
	Clickable     bool   `xml:"clickable,attr"`
	LongClickable bool   `xml:"long-clickable,attr"`
	Focusable     bool   `xml:"focusable,attr"`
	Scrollable    bool   `xml:"scrollable,attr"`
	Enabled       bool   `xml:"enabled,attr"`
	Nodes         []node `xml:"node"`
}

// parseElements returns the dump's interactive nodes in document order and the screen size (the root's bounds).
func parseElements(dump []byte) (els []element, w, h int, err error) {
	var root struct {
		Nodes []node `xml:"node"`
	}
	if err := xml.Unmarshal(dump, &root); err != nil {
		return nil, 0, 0, fmt.Errorf("uiautomator dump: %w (output %q)", err, head(dump))
	}
	for _, n := range root.Nodes {
		_, _, x2, y2, _ := bounds(n.Bounds)
		w, h = max(w, x2), max(h, y2)
		els = walk(n, els)
	}
	return els, w, h, nil
}

// walk appends n and its descendants that an agent can act on; scrollable containers are left to swipe.
func walk(n node, els []element) []element {
	editable := strings.HasSuffix(n.Class, "EditText")
	if x1, y1, x2, y2, ok := bounds(n.Bounds); ok && n.Enabled && !n.Scrollable &&
		(n.Clickable || n.LongClickable || n.Focusable || editable) {
		cls := n.Class[strings.LastIndex(n.Class, ".")+1:]
		els = append(els, element{label: fmt.Sprintf("%s %q", cls, label(n)), x: (x1 + x2) / 2, y: (y1 + y2) / 2})
	}
	for _, c := range n.Nodes {
		els = walk(c, els)
	}
	return els
}

// label is the node's own text or description, else its first descendant's (a list row's title), else its id.
func label(n node) string {
	if s := ownText(n); s != "" {
		return s
	}
	if s := childText(n.Nodes); s != "" {
		return s
	}
	_, id, _ := strings.Cut(n.ID, "/")
	return id
}

func childText(ns []node) string {
	for _, c := range ns {
		if s := ownText(c); s != "" {
			return s
		}
		if s := childText(c.Nodes); s != "" {
			return s
		}
	}
	return ""
}

func ownText(n node) string {
	s := strings.TrimSpace(n.Text)
	if s == "" {
		s = strings.TrimSpace(n.Desc)
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

// bounds parses "[x1,y1][x2,y2]"; ok is false for a malformed or empty box.
func bounds(s string) (x1, y1, x2, y2 int, ok bool) {
	if _, err := fmt.Sscanf(s, "[%d,%d][%d,%d]", &x1, &y1, &x2, &y2); err != nil {
		return 0, 0, 0, 0, false
	}
	return x1, y1, x2, y2, x2 > x1 && y2 > y1
}

// parseFocus reads the foreground window from `dumpsys window`'s "mCurrentFocus=Window{… u0 pkg/activity}".
func parseFocus(out []byte) (pkg, activity string) {
	_, rest, ok := bytes.Cut(out, []byte("mCurrentFocus="))
	if !ok {
		return "", ""
	}
	line, _, _ := bytes.Cut(rest, []byte("\n"))
	f := strings.Fields(strings.TrimRight(string(line), "}\r "))
	if len(f) == 0 || f[0] == "null" {
		return "", ""
	}
	pkg, activity, _ = strings.Cut(f[len(f)-1], "/")
	return pkg, activity
}

// toJPEG re-encodes screencap's PNG, a fraction of its size.
func toJPEG(pngData []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("screencap: %w (output %q)", err, head(pngData))
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil, fmt.Errorf("screencap: %w", err)
	}
	return b.Bytes(), nil
}

// head is the start of a command's output, for an error that shows what came back instead.
func head(b []byte) string { return string(b[:min(len(b), 120)]) }

// shellQuote single-quotes s for the device's sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
