package agent

import "Eylu/internal/protocol"

// HostRuntime contains only explicitly supplied context. Network credentials,
// workspace discovery, local skills and live catalog replacement are forbidden.
type HostRuntime struct {
	Instructions     string
	Context          []string
	ContextCommitted func() error
}

// RequestLifecycle is the durable request boundary shared by CLI/TUI toolRun
// and the stdio host. Storage differs; prepare/commit/settle ordering does not.
type RequestLifecycle struct {
	Started       func(string) error
	UserCommitted func(protocol.Turn) error
	TurnCommitted func(protocol.Turn) error
	ToolPrepared  func(protocol.ToolCall) error
	Finished      func(RunReport) error
	Sync          func(error) error
}

func (l RequestLifecycle) Prepare(options LoopOptions) (LoopOptions, error) {
	options.OnUserCommitted = l.UserCommitted
	options.OnTurnCommitted = l.TurnCommitted
	options.OnToolPrepared = l.ToolPrepared
	if l.Started != nil {
		if err := l.Started(options.RequestID); err != nil {
			return LoopOptions{}, err
		}
	}
	return options, nil
}

func (l RequestLifecycle) Settle(report *RunReport, runErr error) error {
	var reportErr error
	if report != nil && l.Finished != nil {
		reportErr = l.Finished(*report)
	}
	if l.Sync != nil {
		if err := l.Sync(runErr); err != nil {
			return err
		}
	}
	return reportErr
}
