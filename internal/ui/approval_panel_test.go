package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// approvalFixture is one pending approval with a long summary, so the panel has
// more detail than it can always show.
func approvalFixture() *ApprovalRequest {
	return &ApprovalRequest{
		Tool: "bash", Risk: "exec", Summary: "$ go test ./...",
		Reason:       strings.Repeat("Verify safely ", 8),
		PolicyReason: "manual confirmation", Step: 1, Total: 1,
	}
}

// The rejection reason a user types has to be visible while they type it.
//
// The panel is a fixed-height block and the reason is its last rows, so a height
// that does not account for them silently drops the input: the user presses Tab,
// types a reason, and sees nothing. That is the one state the feedback loop exists
// for, so the input row is the last row the panel may give up - detail above it
// goes first.
func TestApprovalRejectionReasonIsVisibleWhileTyping(t *testing.T) {
	for _, height := range []int{12, 16, 18, 24, 30, 40, 60} {
		t.Run(fmt.Sprintf("height=%d", height), func(t *testing.T) {
			model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: height})
			model.operationID = "op-approval"
			model.approval = approvalFixture()
			model.state = StateAwaitingApproval

			if _, _ = model.handleApprovalKey("tab"); !model.approvalEditing {
				t.Fatal("tab did not start the rejection reason")
			}
			_, _ = model.Update(tea.PasteMsg{Content: "use the existing target"})

			view := ansi.Strip(model.View().Content)
			if !strings.Contains(view, "use the existing target") {
				t.Fatalf("the typed rejection reason is not visible at height %d:\n%s", height, view)
			}
			if !strings.Contains(view, "Rejection feedback") {
				t.Fatalf("the reason input has no label at height %d:\n%s", height, view)
			}
			if !strings.Contains(view, "Yes") || !strings.Contains(view, "No") {
				t.Fatalf("the decision choices are missing at height %d:\n%s", height, view)
			}
			if got := lipgloss.Height(view); got > height {
				t.Fatalf("the panel overflows the terminal: %d rows in %d", got, height)
			}
			// The keys that end or leave the edit are the ones the user needs while
			// typing; "Tab add rejection reason" is the hint for a state they are
			// already in.
			if !strings.Contains(view, "Enter reject with it") {
				t.Fatalf("the footer does not say how to submit the reason at height %d:\n%s", height, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if lipgloss.Width(line) > 80 {
					t.Fatalf("a line is wider than the terminal: %q", line)
				}
			}
		})
	}
}

// A reason that has already been typed stays visible after Tab leaves the input, so
// the decision about to be submitted is the one on screen.
func TestApprovalRejectionReasonStaysVisibleAfterLeavingTheInput(t *testing.T) {
	model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: 24})
	model.approval = approvalFixture()
	model.state = StateAwaitingApproval
	if _, _ = model.handleApprovalKey("tab"); !model.approvalEditing {
		t.Fatal("tab did not start the rejection reason")
	}
	_, _ = model.Update(tea.PasteMsg{Content: "split the helper first"})
	if _, _ = model.handleApprovalKey("tab"); model.approvalEditing {
		t.Fatal("tab did not leave the rejection reason")
	}
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "split the helper first") {
		t.Fatalf("the typed reason disappeared when the input lost focus:\n%s", view)
	}
}

// The plan gate holds the same kind of inline input - Tab on Reject asks for a
// revision - and it is clipped the same way unless the panel asks for its rows.
func TestPlanGateFeedbackIsVisibleWhileTyping(t *testing.T) {
	for _, height := range []int{12, 16, 18, 24, 30, 40} {
		t.Run(fmt.Sprintf("height=%d", height), func(t *testing.T) {
			model := NewModel(&fakeBackend{}, Options{NoAnimation: true, NoColor: true, Width: 80, Height: height})
			model.state = StateCompleted
			model.planGate = newPlanGate(model.width)
			model.planGate.cursor = 2

			if _, _ = model.handlePlanGateKey("tab"); !model.planGate.editing {
				t.Fatal("tab did not start the plan feedback")
			}
			_, _ = model.Update(tea.PasteMsg{Content: "keep the public API stable"})

			view := ansi.Strip(model.View().Content)
			if !strings.Contains(view, "keep the public API stable") {
				t.Fatalf("the typed plan feedback is not visible at height %d:\n%s", height, view)
			}
			if !strings.Contains(view, "Plan feedback") {
				t.Fatalf("the feedback input has no label at height %d:\n%s", height, view)
			}
			for _, choice := range []string{"Auto", "Full", "Reject"} {
				if !strings.Contains(view, choice) {
					t.Fatalf("%s is missing at height %d:\n%s", choice, height, view)
				}
			}
			if !strings.Contains(view, "Enter request it") {
				t.Fatalf("the footer does not say how to submit the revision at height %d:\n%s", height, view)
			}
			if got := lipgloss.Height(view); got > height {
				t.Fatalf("the panel overflows the terminal: %d rows in %d", got, height)
			}
		})
	}
}
