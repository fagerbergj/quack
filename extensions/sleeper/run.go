package sleeper

import (
	"cmp"
	"context"
	"fmt"

	"github.com/fagerbergj/quack-extensions/sdk"
)

func (e *extension) markRunning(chatID string) {
	e.runningMu.Lock()
	defer e.runningMu.Unlock()
	if e.running == nil {
		e.running = make(map[string]struct{})
	}
	e.running[chatID] = struct{}{}
}

func (e *extension) clearRunning(chatID string) {
	e.runningMu.Lock()
	defer e.runningMu.Unlock()
	delete(e.running, chatID)
}

func (e *extension) isRunning(chatID string) bool {
	e.runningMu.Lock()
	defer e.runningMu.Unlock()
	_, ok := e.running[chatID]
	return ok
}

// RunEnded clears the running badge for any final outcome and observes a done job; an unknown
// chatID (already cleared, or never dispatched here) is a no-op.
func (e *extension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	e.clearRunning(chatID)
	e.observeRun(chatID, outcome)
}

// jobTitles is the sidebar chip's job word for week-stop jobs; other stops use a fixed label.
var jobTitles = map[string]string{
	"lineup":       "Lineup",
	"waivers":      "Waivers",
	"trends":       "Trends",
	"digest":       "Digest",
	"retro":        "Retro",
	"trade-finder": "Trade finder",
}

// originLabel is the sidebar chip text: "<Title> · week N" for a week job, else a fixed form.
func originLabel(stop, job, partnerName string) string {
	switch {
	case job == "trade":
		return "Trade · " + partnerName
	case stop == "draft":
		return "Draft"
	case stop == "review":
		return "Season review"
	default:
		return jobTitles[job] + " · week " + stop
	}
}

// leagueBadge is best-effort: a failure leaves it "" rather than blocking the dispatch.
func (e *extension) leagueBadge(ctx context.Context, leagueID string) string {
	lg, err := e.client.League(ctx, leagueID)
	if err != nil {
		return ""
	}
	return lg.Name
}

func chatLabels(leagueID, leagueName, job string) map[string][]sdk.LabelValue {
	return map[string][]sdk.LabelValue{
		"league": {{Value: leagueID, Display: cmp.Or(leagueName, leagueID)}},
		"job":    {{Value: job}},
	}
}

// jobOrigin builds the sidebar chip for a job or season-notes dispatch;
// href is a deep link main.js's own query params (league_id, stop) resolve.
func (e *extension) jobOrigin(ctx context.Context, leagueID, stop, job, label string) *sdk.ChatOrigin {
	badge := e.leagueBadge(ctx, leagueID)
	href := "/sleeper/?league_id=" + leagueID
	if stop != "" {
		href += "&stop=" + stop
	}
	return &sdk.ChatOrigin{
		Extension: extensionName,
		Label:     label,
		Kind:      job,
		Href:      href,
		Badge:     badge,
		Labels:    chatLabels(leagueID, badge, job),
	}
}

func globalChatID(localID string) string { return "ext:sleeper:" + localID }

// dispatchTracked marks running before Dispatch: a run can end (firing RunEnded) before a later mark
// would land, leaving a phantom Running badge.
func (e *extension) dispatchTracked(ctx context.Context, req sdk.DispatchRequest, chatID string) error {
	e.markRunning(chatID)
	if err := e.host.Dispatch(ctx, req); err != nil {
		e.clearRunning(chatID)
		return err
	}
	return nil
}

// dispatchSeasonNotes: a trends run also dispatches season notes into their own chat.
func (e *extension) dispatchSeasonNotes(ctx context.Context, leagueID string) {
	localID := leagueID + ":season-notes"
	origin := e.jobOrigin(ctx, leagueID, "", "season-notes", "Season notes")
	err := e.dispatchTracked(ctx, sdk.DispatchRequest{
		Chat: sdk.ChatRef{LocalID: localID, User: e.cfg.DefaultUser, Title: "Sleeper season notes", Origin: origin},
		Ask:  sdk.Ask{Message: fmt.Sprintf("Update the running season notes for league %s from this week's trends findings.", leagueID)},
		// Bound to a workflow to skip the planner LLM call.
		Run: sdk.RunConfig{ReadOnly: true, Workflow: "sleeper-season-notes"},
	}, globalChatID(localID))
	if err != nil && e.host.Log != nil {
		e.host.Log.Error("sleeper: season-notes dispatch failed", "league_id", leagueID, "err", err)
	}
}
