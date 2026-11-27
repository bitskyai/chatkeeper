package core

import (
	"errors"
	"github.com/bitskyai/chatkeeper/pkg/pluginapi"
)

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
	for name := range required {
		if membership[name].Count < 1 {
			return false, nil
		}
	}
	if rule.Who != "owner" {
		return true, nil
	}
	for _, owner := range rule.Owners {
		all := true
		for name := range required {
			found := false
			for _, u := range membership[name].Users {
				if u == owner {
					found = true
				}
			}
			all = all && found
		}
		if all {
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
