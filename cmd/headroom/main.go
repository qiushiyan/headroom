// headroom — Claude Code's /usage view, outside Claude Code, across
// accounts: a cross-account usage dashboard, an interactive account picker,
// and a self-check for the reverse-engineered facts underneath. System
// model and vendor contracts: DESIGN.md.
package main

import (
	"os"

	"github.com/qiushiyan/headroom/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
