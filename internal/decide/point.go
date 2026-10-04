package decide

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/fagerbergj/quack/internal/config"
)

// Point is a named intercept point: the questions its handler answers and how
// the primary one maps onto policy. It makes no call until decisions.points.<id> enables it.
type Point struct {
	ID        string
	Questions map[string]Question
	// Primary is the question whose top answer the policy reads.
	Primary string
	// Restrictive lists Primary's answers a guard may apply; any other answer passes.
	Restrictive []string
	// Replaces names the step an acting decide outcome skips, recorded as skipped_step.
	Replaces string
	// Modes the point's caller implements; nil allows every mode.
	Modes []string
}

// ErrNamespace: an extension named a point outside its own ext:<plugin>/ namespace.
var ErrNamespace = errors.New("decide: point id outside the caller's namespace")

var (
	registryMu sync.RWMutex
	registry   = map[string]Point{}
)

// Register adds a core point and binds its primary answer to T. Call it from a
// package var; a duplicate or ext: id panics at init, since ext: belongs to extensions.
func Register[T any](p Point, parse func(top string) T) Typed[T] {
	if _, ok := p.Questions[p.Primary]; !ok {
		panic(fmt.Sprintf("decide: point %q: primary question %q is not one of its questions", p.ID, p.Primary))
	}
	if strings.HasPrefix(p.ID, config.DecisionExtPrefix) {
		panic(fmt.Sprintf("decide: point %q: the ext: namespace is reserved for extensions", p.ID))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[p.ID]; dup {
		panic(fmt.Sprintf("decide: point %q registered twice", p.ID))
	}
	registry[p.ID] = p
	return Typed[T]{Point: p, Parse: parse}
}

// RegisterObserveNoul registers an observe-only core point asking one noul question, parsed as a bool.
func RegisterObserveNoul(id, question, instructions, replaces string) Typed[bool] {
	return Register(Point{ID: id, Primary: question, Replaces: replaces, Modes: []string{config.DecisionModeObserve},
		Questions: map[string]Question{question: {Type: "noul", Instructions: instructions}}}, func(top string) bool { return top == "true" })
}

// ExtPointID places an extension's point name in its own namespace: a bare
// name becomes ext:<plugin>/<name>; an ext: id must already be the plugin's own.
func ExtPointID(plugin, name string) (string, error) {
	id := name
	if !strings.HasPrefix(name, config.DecisionExtPrefix) {
		id = config.DecisionExtPrefix + plugin + "/" + name
	}
	if !strings.HasPrefix(id, config.DecisionExtPrefix+plugin+"/") || !config.ValidExtPointID(id) {
		return "", fmt.Errorf("%w: %s may only use ext:%s/<name>, got %q", ErrNamespace, plugin, plugin, name)
	}
	return id, nil
}

func lookup(id string) (Point, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[id]
	return p, ok
}

func registeredIDs() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return slices.Sorted(maps.Keys(registry))
}

// Typed is a registered point whose primary answer parses into T.
type Typed[T any] struct {
	Point
	Parse func(top string) T
}

// Decision is a Result with its top answer parsed; Value is T's zero value when there is no answer.
type Decision[T any] struct {
	Result
	Value T
}

// Decide is Decider.Decide with the answer parsed; baseline is quack's own value.
// Decide mode has no settle step: when the point Replaces a step that would produce the baseline,
// a fallback records the caller's placeholder, not that step's real output, so the shadow A/B is not comparable there.
func (t Typed[T]) Decide(ctx context.Context, d *Decider, state any, baseline T) Decision[T] {
	r := d.DecideWith(ctx, t.Point, state, fmt.Sprint(baseline))
	x := Decision[T]{Result: r}
	if r.Top != "" {
		x.Value = t.Parse(r.Top)
	}
	return x
}

// Choose is decide mode: the handler's answer when it acted, else fallback.
func (x Decision[T]) Choose(fallback T) T {
	if x.Act() {
		return x.Value
	}
	return fallback
}

// Guard is guard mode: the handler's answer only when it restricts, else current.
func (x Decision[T]) Guard(current T) T {
	if x.Restricts() {
		return x.Value
	}
	return current
}

// Annotated is a state whose Meta is recorded beside it but never sent to the handler,
// so a flag correlated with the baseline cannot sway the answer.
type Annotated struct {
	State any
	Meta  any
}

const clipMarker = " …[truncated]"

// Clip cuts s to at most n bytes on a rune boundary and marks the cut. States are
// sized in bytes: code and JSON escaping run near 3 bytes per token, not English's 4.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n-len(clipMarker), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + clipMarker
}

const midMarker = " …[truncated]… "

// ClipEnds is Clip keeping the head and the tail, where an answer's sources and conclusions sit.
func ClipEnds(s string, n int) string {
	if len(s) <= n {
		return s
	}
	keep := max(n-len(midMarker), 0)
	head, tail := keep*2/3, len(s)-(keep-keep*2/3)
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + midMarker + s[tail:]
}
