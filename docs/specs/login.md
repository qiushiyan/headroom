# Login — renew every expiring Claude Code login in one pass

Status: built on the `login` branch, 2026-10-06, and tested on the owner's
mini with two real renewals that day; kept as the dated record of the
decision. The design as it stands lives in `DESIGN.md` § Renewing logins —
read that, not this, for what is true today. Written against `main` at
982670c, Claude Code 2.1.291, macOS. Measurements below were taken on the
owner's mini on that date.

## Summary

Current: a Claude Code login ends at its `refreshTokenExpiresAt`, about 27–30 days after `/login`, and using the account does not move that date.
Current: the owner holds six subscriptions on each of two machines, so twelve logins end on twelve different days, and each is renewed by entering a session on that account, typing `/login`, and approving in the browser profile signed in to it.
Current: the board already knows each login's end, and says "relogin required" only once it has passed.

Goal: one command renews every login that ends soon, and the owner's whole part is one Authorize click per account.
Goal: renewing them together leaves them ending together, so the next pass is one batch again.

Change: `headroom login [<name>…] [--all] [--within <days>] [--dry-run]` runs Claude Code's own `claude auth login` for each chosen account, one at a time, under the account's environment.
Change: the approval page opens in the Chrome profile matched to the account's email; with no match it opens in the default browser.

Boundary: headroom still never writes a credential. Claude Code performs the login and stores it; headroom chooses which accounts, opens the page, and reads the result back.
Boundary: the Authorize click stays human. Clicking it by script would mean driving the owner's signed-in browser, and the consent page is the vendor's.
Boundary: Claude Code only. Codex logins are renewed by any run and have no end date to plan by.
Boundary: a login the vendor ended early (seen on the mini 2026-10-06: four logins rejected within 34 minutes, one made six days earlier) shows no evidence in the stored credential; naming the account renews it.

## Vendor facts the command rests on

Measured on 2.1.291:

- `claude auth login --email <e>` is the non-interactive form of `/login`. It
  pre-fills the email on the sign-in page (`login_hint`), starts a localhost
  callback, and also accepts the code at `Paste code here if prompted` when
  the browser cannot reach the callback.
- It opens the page by exec'ing `$BROWSER` with the URL as the only
  argument, inheriting the environment. A `$BROWSER` that carries arguments
  is not run at all, so the value must be a single executable path.
- It honors `CLAUDE_CONFIG_DIR` like every other entry point, so the
  environment `launch.Prepare` builds routes the login to the account's dir
  and Keychain item.

## Choosing the accounts

With names, exactly those accounts. With `--all`, every Claude Code account.
Otherwise an account is chosen on positive evidence only, as the board reads
health: no login, a refresh expiry already passed, or a refresh expiry within
`--within` days (default 7). An account whose credential cannot be read or
whose expiry is absent is listed as skipped with the reason, never chosen by
guess.

## Opening the right browser profile

`$BROWSER` is the headroom binary itself, with `HEADROOM_BROWSER_PROFILE`
set to the chosen Chrome profile directory (empty for the default browser).
Run with that variable and a single https URL as its arguments, headroom does
nothing but open it: `open -na "Google Chrome" --args
--profile-directory=<dir> <url>`, or `open <url>`. This mode dispatches before
configuration, like `version`.

A profile matches an account when Chrome's `Local State` lists the account's
email as the profile's signed-in Google account, or, failing that, when the
profile's display name is the email or its local part, case-insensitively,
and the profile is not signed in to another Google account. Exactly one
profile must match; none or several falls back to the default browser and the
plan says so. The sign-in exclusion is load-bearing on both machines: the
`Default` profile is named `Qiushi` and signed in to the primary's Gmail,
while `qiushi@planlab.ai` lives in a profile named `qiushi`. On both machines today the owner's profiles are named by
local part (`cliushi`, `qiushiyan`, …) and the primary's profile is signed in
to its Google account.

## Reading the result back

After each login, headroom re-reads the credential and `.claude.json`. It
reports the new end date, and fails the account when the credential did not
change or the logged-in email is not the account's — the wrong-profile case,
which the board would otherwise only catch later as `(dir says …!)`.
