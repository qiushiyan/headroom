package accounts

import "github.com/qiushiyan/headroom/internal/config"

// claudeScope is the Claude Code scope under a fixture home, with the
// primary's name pinned the way the fixtures expect it.
func claudeScope(home, primary string) config.Scope {
	s := config.ForHome(home).Claude
	s.PrimaryName = primary
	return s
}
