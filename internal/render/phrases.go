package render

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Tilde is a path as a person writes it: the home directory as ~.
func Tilde(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home+"/") {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// When is an instant as a date plus its distance from now: "Oct 30 (in 6d)".
func When(ms int64, now time.Time) string {
	t := time.UnixMilli(ms)
	d := t.Sub(now)
	if d < 0 {
		return t.Local().Format("Jan 2") + " (" + Days(-d) + " ago)"
	}
	return t.Local().Format("Jan 2") + " (in " + Days(d) + ")"
}

// Days is a span in whole days, or whole hours under a day: "6d", "20h".
func Days(d time.Duration) string {
	if d < 24*time.Hour {
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// Span is a wait in words: "10 minutes".
func Span(d time.Duration) string {
	if d < time.Minute {
		return d.String()
	}
	if m := int(d.Minutes()); m != 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "1 minute"
}

// LogWhen is one of the logs' RFC3339 instants as the launch surfaces print
// it: the local date and minute. A time that does not parse is printed as it
// stands.
func LogWhen(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("01-02 15:04")
	}
	return s
}
