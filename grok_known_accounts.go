package main

import (
	"errors"
	"net/http"
	"sort"
	"time"
)

// KnownGrokAccountUsage is one parked Grok snapshot's billing snapshot.
// Leaner than KnownAccountUsage: no Reason/Expired/Verified/backoff.
type KnownGrokAccountUsage struct {
	Name      string         `json:"name"`
	Account   string         `json:"account"`
	Info      *GrokUsageInfo `json:"info"`
	Stale     bool           `json:"stale,omitempty"`
	FetchedAt time.Time      `json:"fetchedAt,omitzero"`
}

type knownGrokAccountsResult struct {
	Accounts []KnownGrokAccountUsage
}

type KnownGrokAccountsHub = usagePoller[knownGrokAccountsResult]

func NewKnownGrokAccountsHub() *KnownGrokAccountsHub {
	return newUsagePoller(nil, fetchKnownGrokAccounts, func(*knownGrokAccountsResult) {})
}

func derefKnownGrokAccounts(res *knownGrokAccountsResult) []KnownGrokAccountUsage {
	if res == nil {
		return nil
	}
	return res.Accounts
}

// fetchKnownGrokAccounts walks parked grok snapshots one at a time and
// fetches billing for each. The live email is skipped (do not rotate a
// parked copy of the live grant). A per-account failure omits that account
// this pass rather than failing the batch.
func fetchKnownGrokAccounts() (*knownGrokAccountsResult, error) {
	names, err := grokSnapshotNames()
	if err != nil {
		return &knownGrokAccountsResult{}, nil
	}
	live := liveGrokEmail()
	accounts := make([]KnownGrokAccountUsage, 0, len(names))
	for _, name := range names {
		email := grokSnapshotGrantEmail(name)
		if emailMatchesLive(email, live) {
			continue
		}
		tok, err := grokSnapshotAccessToken(name)
		if err != nil || tok == "" {
			continue
		}
		u, err := fetchGrokUsageWith(tok, email)
		if err != nil {
			var he *grokHTTPError
			if errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden) {
				if rotated := tryRotateGrokSnapshot(name); rotated != "" {
					u, err = fetchGrokUsageWith(rotated, email)
				}
			}
			if err != nil || u == nil || u.Info == nil {
				continue
			}
		}
		if u == nil || u.Info == nil {
			continue
		}
		accounts = append(accounts, KnownGrokAccountUsage{
			Name:      name,
			Account:   email,
			Info:      u.Info,
			FetchedAt: time.Now(),
		})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return &knownGrokAccountsResult{Accounts: accounts}, nil
}
