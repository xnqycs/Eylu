package policy

import (
	"fmt"
	"sort"
	"strings"
)

// SafetySettings is the safety-relevant part of the settings one request runs
// under.
//
// The host keeps one of these for the request that is running. When it changes a
// setting it compares the new snapshot with the old one, because the two cases
// are not the same: a change that narrows what the request may do has to reach
// the request that is already running, while a change that widens it belongs to
// the next request only. A running request is never granted a permission it did
// not have when it started.
type SafetySettings struct {
	// Mode is the permission mode.
	Mode PermissionMode
	// WebPermission is the hosted-web permission: "allow", "ask" or "deny". The
	// empty value is the documented default and ranks as "allow".
	WebPermission string
	// MCPServers is the set of enabled MCP servers. Losing one narrows the tools
	// the request may use.
	MCPServers []string
	// DeniedTools is the set of tools that may never run. Gaining one narrows the
	// request.
	DeniedTools []string
}

// Tightening reports why next is narrower than previous, or an empty string when
// it is not. It is deliberately one-sided: widening, or a change that does not
// affect safety, returns nothing so the running request is left alone.
//
// The first reason found is returned, in the order mode, hosted-web permission,
// denied tools, MCP servers, so a failure message names the setting a human
// would change first.
func Tightening(previous, next SafetySettings) string {
	if next.Mode < previous.Mode {
		return fmt.Sprintf("permission mode %s -> %s", previous.Mode, next.Mode)
	}
	if webRank(next.WebPermission) < webRank(previous.WebPermission) {
		return fmt.Sprintf("hosted web permission %s -> %s", webPermissionLabel(previous.WebPermission), webPermissionLabel(next.WebPermission))
	}
	if gained := difference(next.DeniedTools, previous.DeniedTools); len(gained) > 0 {
		return fmt.Sprintf("denied tools now include %s", strings.Join(gained, ", "))
	}
	if lost := difference(previous.MCPServers, next.MCPServers); len(lost) > 0 {
		return fmt.Sprintf("MCP servers no longer enabled: %s", strings.Join(lost, ", "))
	}
	return ""
}

// webRank orders the hosted-web permissions from narrowest to widest. An
// unrecognized value ranks as the narrowest, so a permission this build does not
// understand can only ever stop a request, never silently widen one.
func webRank(permission string) int {
	switch permission {
	case "", "allow":
		return 2
	case "ask":
		return 1
	case "deny":
		return 0
	default:
		return 0
	}
}

func webPermissionLabel(permission string) string {
	if permission == "" {
		return "allow (default)"
	}
	return permission
}

// difference returns the entries of a that are not in b, sorted so the message
// does not depend on map iteration order.
func difference(a, b []string) []string {
	if len(a) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(b))
	for _, item := range b {
		seen[item] = struct{}{}
	}
	var result []string
	for _, item := range a {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; !ok {
			result = append(result, item)
		}
	}
	sort.Strings(result)
	return result
}
