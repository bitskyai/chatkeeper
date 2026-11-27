package core

import (
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

// MatchAny reports whether a rule archives when any one listed reaction is present.
// Rules without an explicit mode keep the legacy all-reactions semantics.
func MatchAny(rule pluginapi.Trigger) bool { return rule.Match == "any" }

func Matches(rule pluginapi.Trigger, root pluginapi.Root) (bool, error) {
	if root.TS == "" || root.ThreadTS != "" && root.ThreadTS != root.TS || len(rule.Reactions) == 0 {
		return false, nil
	}
	required := map[string]bool{}
	for _, name := range rule.Reactions {
		required[name] = true
	}
	membership := map[string]pluginapi.Reaction{}
	for _, r := range root.Reactions {
		if !required[r.Name] {
			continue
		}
		if _, ok := membership[r.Name]; ok || r.Count < len(r.Users) || r.Count < 0 {
			return false, errors.New("invalid reaction membership")
		}
		membership[r.Name] = r
	}
	present := 0
	for name := range required {
		if membership[name].Count >= 1 {
			present++
		}
	}
	any := MatchAny(rule)
	if present == 0 || !any && present < len(required) {
		return false, nil
	}
	if rule.Who != "owner" {
		return true, nil
	}
	for _, owner := range rule.Owners {
		supplied := 0
		for name := range required {
			for _, u := range membership[name].Users {
				if u == owner {
					supplied++
					break
				}
			}
		}
		if any && supplied > 0 || !any && supplied == len(required) {
			return true, nil
		}
	}
	for _, r := range membership {
		unique := map[string]bool{}
		for _, u := range r.Users {
			unique[u] = true
		}
		if r.Count > len(unique) {
			return false, errors.New("incomplete owner reaction membership")
		}
	}
	return false, nil
}
