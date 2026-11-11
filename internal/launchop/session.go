package launchop

import (
	"regexp"
	"strings"
)

// Session is what a launch's pass-through arguments say about which
// session it is for. It is read, never rewritten: the arguments reach the
// vendor exactly as they were given.
//
// Route is the session whose owner the launch should follow; Record is the
// session to re-home when the launch is placed somewhere else. They differ on
// purpose. A fork follows the session it forks from and creates another, so
// nothing may be recorded about the original; a first turn names an id that
// nobody owns yet, and recording it is what lets its later turns follow.
//
// Unidentified means the arguments name a session headroom cannot identify —
// `--continue`, or a bare `--resume` that opens the vendor's own picker. Which
// session those choose is the vendor's rule, unverified here, so nothing is
// inferred and nothing is recorded: a guessed id written down as a routing
// record would misattribute a session for as long as the record lived.
type Session struct {
	Route        string
	Record       string
	Unidentified bool
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// SessionOf is the one reader of the vendor's session flags in a launch's
// arguments. An id counts only when it has the shape of one: the token after
// `--resume` may equally be a search term, and a value that merely follows the
// flag is not thereby a session. It stops at a bare `--`, after which
// everything is the vendor's positional text.
//
// A scanner cannot know which of the vendor's other options take a value, so a
// session flag appearing *as* another option's value would be read as one. An
// id-shaped token must still follow it, and the consequence of a false match
// is a launch routed to that session's account — a valid account either way.
func SessionOf(args []string) Session {
	var resume, fresh string
	var ref Session
	fork := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch {
		case a == "--continue" || a == "-c":
			ref.Unidentified = true
		case a == "--resume" || a == "-r":
			if uuidShape.MatchString(next) {
				resume = next
				i++
			} else {
				ref.Unidentified = true
			}
		case len(a) > len("--resume=") && a[:len("--resume=")] == "--resume=":
			if v := a[len("--resume="):]; uuidShape.MatchString(v) {
				resume = v
			} else {
				ref.Unidentified = true
			}
		case a == "--session-id":
			if uuidShape.MatchString(next) {
				fresh = next
				i++
			}
		case len(a) > len("--session-id=") && a[:len("--session-id=")] == "--session-id=":
			if v := a[len("--session-id="):]; uuidShape.MatchString(v) {
				fresh = v
			}
		case a == "--fork-session":
			fork = true
		}
	}
	switch {
	case ref.Unidentified:
		return Session{Unidentified: true}
	case resume != "" && fresh != "":
		// A fork given its own id: follow the session it forks from.
		return Session{Route: resume}
	case resume != "" && fork:
		return Session{Route: resume}
	case resume != "":
		return Session{Route: resume, Record: resume}
	case fresh != "":
		return Session{Route: fresh, Record: fresh}
	}
	return Session{}
}

// ModelsOf is the one reader of the vendor's model flag in a launch's
// arguments: every value given to `--model`, in order, up to a bare `--`.
// Which of several the vendor honours is its own rule, unread here, so the
// rule is handed them all and is admitted by the first it finds a limit for
// — a launch that might run on a limited model is held to that limit. As with
// SessionOf, a flag appearing as another option's value would be read as one;
// the consequence is a launch held to a limit it may not spend, never one let
// past it. `--fallback-model` is another flag and is not read.
func ModelsOf(args []string) []string {
	var models []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return models
		case a == "--model":
			if i+1 < len(args) {
				models = append(models, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--model="):
			models = append(models, strings.TrimPrefix(a, "--model="))
		}
	}
	return models
}
