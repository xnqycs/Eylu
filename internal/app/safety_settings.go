package app

import (
	"Eylu/internal/config"
	"Eylu/internal/policy"
)

// safetySettings snapshots the safety-relevant settings one request runs under.
//
// The backend keeps the snapshot a running request started with, so a later
// change can be compared with it instead of each mutation site having to
// remember the previous value.
func safetySettings(cfg config.Config, opts chatOptions) policy.SafetySettings {
	modeName := cfg.PermissionMode
	if opts.mode != "" {
		modeName = opts.mode
	}
	mode, err := policy.ParseMode(modeName)
	if err != nil {
		// An unusable mode is not a licence: it reads as the narrowest one.
		mode = policy.ModeManual
	}
	settings := policy.SafetySettings{Mode: mode}
	if providerConfig, ok := cfg.Providers[cfg.ActiveProvider]; ok {
		settings.WebPermission = providerConfig.WebTools.Permission
	}
	for _, server := range cfg.MCPServers {
		settings.DeniedTools = append(settings.DeniedTools, server.DenyTools...)
	}
	return settings
}

// beginSafetyBaseline records the settings the request that is about to run
// starts with. It is called on every request so a narrowing can be told apart
// from the settings the request was admitted under.
func (b *tuiBackend) beginSafetyBaseline() {
	b.mu.Lock()
	baseline := safetySettings(b.manager.Config(), b.opts)
	b.safetyBaseline, b.safetyBaselineSet = baseline, true
	b.mu.Unlock()
}

// endSafetyBaseline forgets the running request's settings.
func (b *tuiBackend) endSafetyBaseline() {
	b.mu.Lock()
	b.safetyBaselineSet = false
	b.mu.Unlock()
}

// stopIfTightened stops the request that is running when the settings in effect
// now narrow what it started with, and reports the reason.
//
// It returns an empty string when nothing narrowed or when no request is running,
// so a widening only ever affects the next request. Every mutation site calls it
// after changing a setting; the comparison itself lives in one place.
func (b *tuiBackend) stopIfTightened() string {
	b.mu.Lock()
	baseline, hasBaseline := b.safetyBaseline, b.safetyBaselineSet
	opts := b.opts
	b.mu.Unlock()
	if !hasBaseline {
		return ""
	}
	reason := policy.Tightening(baseline, safetySettings(b.manager.Config(), opts))
	if reason == "" {
		return ""
	}
	if !b.conversation.RequestStop(reason) {
		return ""
	}
	// The request is ending on the new settings, so the baseline moves with it:
	// a second change is compared with the settings this request actually ends
	// under.
	b.mu.Lock()
	b.safetyBaseline = safetySettings(b.manager.Config(), opts)
	b.mu.Unlock()
	return reason
}

// stopForNarrowedTools stops the running request when a tool that was available
// to it is no longer allowed.
func (b *tuiBackend) stopForNarrowedTools(reason string) bool {
	if reason == "" {
		return false
	}
	if !b.conversation.RequestStop(reason) {
		return false
	}
	b.mu.Lock()
	b.safetyBaseline = safetySettings(b.manager.Config(), b.opts)
	b.mu.Unlock()
	return true
}
