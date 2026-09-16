// Package testfault assembles the host dependencies a request can lose so that
// a test can fail one of them - or several at once - without a test-only branch
// in production code and without sleeping to guess at timing.
//
// Every injection surface is a replacement for a dependency the production code
// already takes from its caller:
//
//	store       the two durable halves of a session (Append, Save)
//	audit       tool.AuditSink
//	event       the host event consumer (driver.EmitFunc)
//	checkpoint  tool.CheckpointSink
//	driver      driver.ModelDriver, including timeouts and response rewrites
//
// Because the faulted value is installed where production installs the real
// one, an injection point cannot drift into a branch that only tests exercise.
// The tests in internal/app install these values on the same fields the app
// wires at run time.
//
// # Schedules
//
// A Fault counts the calls of one dependency and applies a Schedule to them.
// The three shapes the hardening plan asks for are all schedules:
//
//	Always()   "持续失败": every call fails
//	Nth(n)     "第 N 次调用失败": only the n-th call fails
//	FirstN(n)  "失败后恢复": the first n calls fail and the rest succeed
//
// AfterN, Between, Any and All compose them, so one dependency can carry two
// independent faults (for example: the first call fails and then every fifth
// one does).
//
// # Coverage matrix
//
// The matrix records which combinations of host failures have a case, which PR
// owns each one, and where the case lives. It is the input for PR-21
// (per-turn persistence), PR-23 (checkpoint cost and failure semantics) and
// PR-29 (soak), which extend it.
//
//	| combination                          | owner  | case                                                              |
//	|--------------------------------------|--------|-------------------------------------------------------------------|
//	| store.Append x store.Save            | PR-16  | app: TestStoreAndSnapshotFailuresTogetherStayIdempotent            |
//	| checkpoint.RecordIntent x cancel     | PR-16  | app: TestCheckpointIntentFailureCancelsTheRestOfTheBatch           |
//	| checkpoint.RecordCompletion x cancel | PR-16  | app: TestCheckpointCompletionFailureDuringACancellationKeepsBothCauses |
//	| driver timeout x recovery retry      | PR-16  | app: TestProviderTimeoutSurvivesARestartWithoutReplaying           |
//	| audit panic x event sink             | PR-18  | app: TestAuditPanicTodayPreemptsTheEventSink                       |
//	| audit panic (isolated) x event sink  | PR-18  | pending: the panic must stop escaping                              |
//	| store.Append x checkpoint            | PR-21  | pending: per-turn persistence                                      |
//	| store.Append x checkpoint x cancel   | PR-21  | pending: the four interruption points                              |
//	| checkpoint.RecordCompletion x append | PR-23  | pending: the compensation path                                     |
//	| every surface x repetition           | PR-29  | pending: soak                                                      |
//
// The cases marked "pending" are deliberately not written yet: they assert
// behaviour that the owning PR introduces, and a case that cannot fail before
// its fix would not prove anything.
package testfault
