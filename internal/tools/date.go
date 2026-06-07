package tools

import (
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type currentDateResult struct {
	Date    string `json:"date"`
	Weekday string `json:"weekday"`
}

func currentDate() (currentDateResult, error) {
	now := time.Now()
	return currentDateResult{
		Date:    now.Format("2006-01-02"),
		Weekday: now.Weekday().String(),
	}, nil
}

func newCurrentDate(_ Deps) (tool.Tool, error) {
	return functiontool.New[struct{}, currentDateResult](
		functiontool.Config{
			Name:        "current_date",
			Description: "Returns today's date. Call this before any research so your queries use the correct year.",
		},
		func(_ tool.Context, _ struct{}) (currentDateResult, error) {
			return currentDate()
		},
	)
}
