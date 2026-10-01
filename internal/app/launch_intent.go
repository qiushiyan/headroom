package app

import "regexp"

// sessionRef is what a launch's pass-through arguments say about which
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
type sessionRef struct {
	Route        string
	Record       string
	Unidentified bool
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// sessionIntent is the one reader of the vendor's session flags in a launch's
// arguments. An id counts only when it has the shape of one: the token after
// `--resume` may equally be a search term, and a value that merely follows the
// flag is not thereby a session. It stops at a bare `--`, after which
// everything is the vendor's positional text.
//
// A scanner cannot know which of the vendor's other options take a value, so a
// session flag appearing *as* another option's value would be read as one. An
// id-shaped token must still follow it, and the consequence of a false match
// is a launch routed to that session's account — a valid account either way.
func sessionIntent(args []string) sessionRef {
	var resume, fresh string
	var ref sessionRef
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
		return sessionRef{Unidentified: true}
	case resume != "" && fresh != "":
		// A fork given its own id: follow the session it forks from.
		return sessionRef{Route: resume}
	case resume != "" && fork:
		return sessionRef{Route: resume}
	case resume != "":
		return sessionRef{Route: resume, Record: resume}
	case fresh != "":
		return sessionRef{Route: fresh, Record: fresh}
	}
	return sessionRef{}
}
