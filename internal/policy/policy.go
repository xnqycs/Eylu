package policy

import (
	"context"
	"encoding/json"
)

type Risk string

const (
	RiskRead  Risk = "read"
	RiskWrite Risk = "write"
	RiskExec  Risk = "exec"
	RiskHigh  Risk = "high"
)

type Decision string

const (
	DecisionAllow   Decision = "allow"
	DecisionConfirm Decision = "confirm"
	DecisionDeny    Decision = "deny"
)

type Request struct {
	Tool      string
	Input     json.RawMessage
	Workspace string
	Risk      Risk
}

type Outcome struct {
	Decision Decision
	Reason   string
	Risk     Risk
}

type Checker interface {
	Check(context.Context, Request) Outcome
}

type BaselineChecker struct{}

func (BaselineChecker) Check(_ context.Context, request Request) Outcome {
	if request.Risk == RiskRead {
		return Outcome{Decision: DecisionAllow, Risk: request.Risk, Reason: "read-only workspace operation"}
	}
	return Outcome{Decision: DecisionConfirm, Risk: request.Risk, Reason: "mutating and command tools require confirmation"}
}

type AllowAllChecker struct{}

func (AllowAllChecker) Check(_ context.Context, request Request) Outcome {
	return Outcome{Decision: DecisionAllow, Risk: request.Risk, Reason: "explicit test or application approval"}
}
