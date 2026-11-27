package core

// The user question queue (internal/queue): every pending question for the
// user, from any session, in one place — open AskUserQuestion prompts,
// permission requests, coordinators' `amux agent goal ask` questions, and
// decision_needed items in the global inbox. Live entries are derived by the
// daemon from runtime state at read time; nothing stores them.
const (
	// QueryQueue reads the queue, its counts, the global inbox counts and the
	// workgroup summary (queue.Overview). Host, the admin console (read-only)
	// and the overseer may read it. Fields (optional): QueueAllField "1" also
	// lists dismissed entries.
	QueryQueue = "queue"
	// ActionQueueAnswer answers one entry; the daemon delivers the answer to the
	// asking session on the host's (the user's) authority after re-checking the
	// entry against fresh state. Host only. Fields: QueueEntryField, and per
	// kind: QueueTextField (question, goal_input, decision_needed) or
	// QueueChoiceField (question: option labels, newline-separated), or
	// QueueAnswersField (question: one answer per question, JSON — see
	// queue.Answer), or SteerDecision allow|deny (permission).
	ActionQueueAnswer = "queue-answer"
	// ActionQueueDismiss hides an entry from the queue: a decision_needed item
	// is dismissed; a live question is hidden until it changes. Host only.
	// Fields: QueueEntryField.
	ActionQueueDismiss = "queue-dismiss"
)

// Queue fields.
const (
	QueueEntryField  = "entry"
	QueueTextField   = "text"
	QueueChoiceField = "choice"
	// QueueAnswersField carries a question entry's per-question answers as a
	// JSON array of queue.Answer ({"choice":[labels],"other":"text"}), in the
	// questions' order. It replaces QueueChoiceField and QueueTextField.
	QueueAnswersField = "answers"
	QueueAllField     = "all"
)
