package state

import "github.com/qiushiyan/headroom/internal/config"

// rootScope is the Claude Code scope a store under root is opened from, at
// the default spacing.
func rootScope(root string) config.Scope {
	return config.Scope{Vendor: config.Claude, AccountsRoot: root, Spacing: config.DefaultSpacing}
}
