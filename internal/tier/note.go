// Package tier computes cpi-idle-operator's desired CPU-tier state for a
// pod, purely from its spec and annotations (see desired.go). It never
// reads or writes a cgroup; the caller is responsible for reconciling the
// computed State against the pod's actual cgroup files.
package tier

import "github.com/azalio/cpi-idle-operator/internal/observe"

// Note codes reuse observe.TierApplyReason rather than a parallel
// vocabulary. The caller feeds a Note's Code straight into
// observe.Recorder's reason argument (typically Recorder.Inactive), so the
// cpi_tier_apply_total "reason" label and the reasons this package can
// report never drift apart: a second, differently-spelled enum here would
// recreate the exact bounded-cardinality problem TierApplyReason exists to
// prevent (HC-5). NoteUnknownTierValue and NoteNoCPULimit are the closed
// set of codes Desired can produce; their local names mirror the tier
// package's own vocabulary for the same conditions.
const (
	// NoteUnknownTierValue mirrors observe.TierApplyReasonValueUnknown: the
	// tier annotation carried a non-empty value this operator does not
	// recognize.
	NoteUnknownTierValue = observe.TierApplyReasonValueUnknown
	// NoteNoCPULimit mirrors observe.TierApplyReasonLimitsCPUMissing: the
	// burst annotation is present but the pod has no positive CPU limit to
	// burst from.
	NoteNoCPULimit = observe.TierApplyReasonLimitsCPUMissing
)

// Note is a non-error condition Desired discovered while computing a pod's
// desired tier state. Desired's postcondition is that it never returns an
// error (see its doc comment); anything that keeps a requested tier from
// taking effect — an unrecognized value, a missing CPU limit — is reported
// here instead, for the caller to translate into the matching
// observe.Recorder outcome and Event.
type Note struct {
	// Code is the closed-vocabulary reason this note exists, drawn from
	// observe.TierApplyReason.
	Code observe.TierApplyReason
	// Message is a human-readable description, safe to embed directly in an
	// Event or log line.
	Message string
}
