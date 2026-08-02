package main

import (
	"fmt"
	"sort"
	"strings"
)

// `account list`'s data model and table rendering. Everything here is pure
// except localAccountListing's own file reads, so the layout is testable without
// a network, a server, or a real ~/.claude.

// accountRow is one account snapshot on one host: its name, the email it
// belongs to, and whether that host is currently logged into it.
type accountRow struct {
	Host   string
	Name   string
	Email  string
	Active bool
}

// accountListing is one host's contribution to the table: its rows, or the
// reason there are none. An unreachable remote is a listing with an Error, never
// a reason to abort the whole command — the same per-host tolerance
// cmdListSessions already has.
type accountListing struct {
	Host  string
	Rows  []accountRow
	Error string
}

// localAccountHost is the label the local machine's rows carry in the table.
const localAccountHost = "local"

// localAccountListing reads this machine's snapshots: every name, its stored
// email, and whether that email is the one live in ~/.claude.json. Matching goes
// through emailMatchesLive, so `account list` and the known-accounts poller can
// never disagree about which snapshot is active.
func localAccountListing() accountListing {
	names, err := snapshotAccountNames()
	if err != nil {
		return accountListing{Host: localAccountHost, Error: err.Error()}
	}
	live := loadAccountEmail()
	rows := make([]accountRow, 0, len(names))
	for _, name := range names {
		email := snapshotAccountEmail(name)
		rows = append(rows, accountRow{
			Host:   localAccountHost,
			Name:   name,
			Email:  email,
			Active: emailMatchesLive(email, live),
		})
	}
	return accountListing{Host: localAccountHost, Rows: rows}
}

// accountSnapshot is one host's account picture exactly as the pollers already
// hold it: the live account's usage (carrying its email), the other accounts it
// holds snapshots for, and the snapshot name standing for the live one. Both the
// `account list` table and the Ctrl+W picker are built from this, so neither ever
// issues a fetch of its own.
type accountSnapshot struct {
	Usage      *AccountUsage
	Known      []KnownAccountUsage
	ActiveName string
}

// accountSnapshotOf reads one remote host's poll result as an accountSnapshot.
func accountSnapshotOf(r RemoteResult) accountSnapshot {
	return accountSnapshot{Usage: r.Usage, Known: r.KnownAccounts, ActiveName: r.ActiveSnapshotName}
}

// accountRowsFrom is the union every consumer needs: the snapshots a host
// reported (knownAccounts, which by construction excludes whichever account is
// live there) plus activeSnapshotName, whose email comes from the live usage
// snapshot rather than a snapshot file. Sorted by name so repeated renders agree,
// and deduped with the active row winning, so a host that ever reports its live
// account in both places still yields one line.
func accountRowsFrom(host string, snap accountSnapshot) []accountRow {
	byName := make(map[string]accountRow, len(snap.Known)+1)
	for _, a := range snap.Known {
		byName[a.Name] = accountRow{Host: host, Name: a.Name, Email: a.Account}
	}
	if snap.ActiveName != "" {
		email := ""
		if snap.Usage != nil {
			email = snap.Usage.Account
		}
		byName[snap.ActiveName] = accountRow{
			Host:   host,
			Name:   snap.ActiveName,
			Email:  email,
			Active: true,
		}
	}
	rows := make([]accountRow, 0, len(byName))
	for _, row := range byName {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// remoteAccountListing turns one host's reported accounts into its rows. It
// reads nothing but the three account fields, which is why the CLI paths fetch
// /usage alone for it and never poll /sessions.
func remoteAccountListing(r RemoteResult) accountListing {
	if r.Error != "" {
		return accountListing{Host: r.Name, Error: r.Error}
	}
	return accountListing{Host: r.Name, Rows: accountRowsFrom(r.Name, accountSnapshotOf(r))}
}

// renderAccountTable formats the listings as one padded table, hosts in the
// order given. A listing carrying an Error prints that error where its rows
// would have been, so one unreachable host costs one line rather than the whole
// table. A host with no snapshots at all prints a placeholder for the same
// reason: silence would read as a failed command.
func renderAccountTable(listings []accountListing) string {
	const (
		hostHdr   = "HOST"
		nameHdr   = "NAME"
		emailHdr  = "EMAIL"
		activeHdr = "ACTIVE"
	)
	hostW, nameW, emailW := len(hostHdr), len(nameHdr), len(emailHdr)
	for _, l := range listings {
		if n := len(l.Host); n > hostW {
			hostW = n
		}
		for _, row := range l.Rows {
			if n := len(row.Name); n > nameW {
				nameW = n
			}
			if n := len(displayEmail(row.Email)); n > emailW {
				emailW = n
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %s\n", hostW, hostHdr, nameW, nameHdr, emailW, emailHdr, activeHdr)
	for _, l := range listings {
		if l.Error != "" {
			fmt.Fprintf(&b, "%-*s  %s\n", hostW, l.Host, l.Error)
			continue
		}
		if len(l.Rows) == 0 {
			fmt.Fprintf(&b, "%-*s  %s\n", hostW, l.Host, "no account snapshots")
			continue
		}
		for _, row := range l.Rows {
			active := "no"
			if row.Active {
				active = "yes"
			}
			fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %s\n",
				hostW, row.Host, nameW, row.Name, emailW, displayEmail(row.Email), active)
		}
	}
	return b.String()
}

// displayEmail renders an unknown email as "-" rather than blank, so the ACTIVE
// column never floats next to empty space.
func displayEmail(email string) string {
	if email == "" {
		return "-"
	}
	return email
}
