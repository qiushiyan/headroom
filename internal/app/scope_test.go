package app

import "github.com/qiushiyan/headroom/internal/config"

// claudeScope is the Claude Code scope under a fixture home, with the
// primary's name pinned the way the fixtures expect it.
func claudeScope(home, primary string) config.Scope {
	s := config.ForHome(home).Claude
	s.PrimaryName = primary
	return s
}

func withUsageURL(s config.Scope, url string) config.Scope {
	s.UsageURL = url
	return s
}

// claudeBoard wraps a list the way a Claude Code-only run reports it.
func claudeBoard(list []*accountData, current string) []vendorBoard {
	return []vendorBoard{{scope: config.Scope{Vendor: config.Claude}, list: list, current: current}}
}
