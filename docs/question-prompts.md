# Question prompts v2: parity with Claude Code and Codex (design)

Status: approved design (0caeb5f); phase 2 implementation in progress.
Workgroup "question prompt parity". **This document is the cross-repo
contract** for harness and harness-android. Every schema change lands here in
the same commit; see "Contract changelog" at the end.
Scope: agent-multiplexer (daemon, TUI, CLI, direct), harness (web and cloud
relay), harness-android.

**Goal.** A question that an agent asks through amux must be answerable from
any amux surface with everything the harness supports in its own pane:
1–4 questions, headers, options with descriptions, multi-select, "Other" free
text, option previews, per-question notes, and cancel. The answer must
reach the agent as a structured answer, not as a prompt typed in its place.
The question must clear everywhere once it is answered anywhere, including
in the pane.

The surfaces are the native TUI, the console/overseer queue, `amux` CLI,
web (harness) and Android (harness-android).

---

## 1. Current state

### 1.1 What the harnesses offer

**Claude Code** (installed: 2.1.330). Sources:

- Its own tool schema: the AskUserQuestion definition Claude Code hands the
  model in this very session.
- The cached changelog (`$CLAUDE_CONFIG_DIR/cache/changelog.md`).
- The official hooks, settings and tools references
  (code.claude.com/docs/en/hooks, /settings-reference, /tools-reference,
  /agent-sdk/user-input).

The `claude` binary is not runnable from the agent sandbox. The points
marked **(fixture)** in §7 must therefore be captured on a host before
implementation relies on them.

| Feature | Claude Code 2.1.330 | Source |
| --- | --- | --- |
| 1–4 questions per call | `questions` minItems 1, maxItems 4 | tool schema; hooks reference |
| header chip | `header`, ≤12 chars | tool schema |
| 2–4 options, label + description | `options[{label, description, preview?}]`, minItems 2, maxItems 4 | tool schema |
| multi-select | `multiSelect` (default false) | tool schema |
| automatic "Other" | always offered by the dialog, never in `options`; free text | tool schema ("Users will always be able to select Other") |
| previews | `options[].preview`, single-select only, shown side by side | tool schema; changelog "AskUserQuestion preview…" (lines 4727-4728, 7233) |
| notes | `annotations{<question text>: {notes?, preview?}}`; the TUI's notes field is offered in preview mode | tool schema; changelog 2425, 4727, 7829 |
| "(Recommended)" | a label convention, recommended option first | changelog 10607 |
| "Chat about this" | a dialog row that closes the dialog and returns to the composer | changelog 8121 |
| answers | `answers{<question text>: "<label>[, <label>…]" \| "<Other text>"}`; arrays are also accepted for multi-select | tool schema; changelog 8107; SDK user-input doc |
| cancel | Esc closes the dialog. In vim mode Esc in the "Other"/notes field first enters NORMAL, and Esc cancels only from NORMAL | `agent/keys.go:135-139`; changelog 2561, 7829 |
| auto-continue timeout | setting `askUserQuestionTimeout`: `"60s" \| "5m" \| "10m" \| "never"`, default `never`. On expiry it submits whatever is selected and tells Claude the user may be away. The timer never runs in background sessions or under Remote Control | settings reference; changelog 6940, 2188 |
| **structured answer channel** | **PreToolUse hook for `AskUserQuestion` returning `{permissionDecision:"allow", updatedInput:{questions, answers, annotations?}}`** answers without the dialog. "allow" alone is not enough | changelog 9076; hooks reference "Tools that require user interaction" |
| input validation of `updatedInput` | validated like Claude's own input. Unknown keys are dropped, and input the tool rejects denies the call | changelog 2657 |
| hook blocking | command hooks block by default. Per-hook `timeout` is in seconds, default 600. Behaviour at timeout is undocumented **(fixture)** | hooks reference |
| PermissionRequest for AskUserQuestion | undocumented whether it fires **(fixture)** | hooks reference |
| Notification type for an open question | undocumented. `elicitation_dialog` is documented for MCP only **(fixture)** | hooks reference |
| tool_result text on answer / Esc / timeout | undocumented **(fixture)**. Free-text answers get "neutral wording" | changelog 6514 |

**Codex** (app-server JSON-RPC, verified against amux code and the harness
pilot notes; the schema itself is not on disk):

- **Approvals.** The methods are `item/commandExecution/requestApproval` and
  `item/fileChange/requestApproval`. Their decision enum is
  `accept | acceptForSession | decline | cancel`. The command variant also
  has the object forms `acceptWithExecpolicyAmendment` and
  `applyNetworkPolicyAmendment`. Legacy `execCommandApproval` and
  `applyPatchApproval` take a `ReviewDecision`. Source:
  `harness/backend/runtime/codexapp/session.go:36-69`.
- **User input.** Params: `{threadId, turnId, itemId, questions:[{id, header,
  question, isOther, isSecret, options:[{label, description}]|null}]}`.
  Response: `{answers:{<question id>:{answers:[string]}}}`. Sources:
  `harness/docs/codex-app-server-pilot.md:218-232` and
  `harness/backend/runtime/codexapp/approvals_test.go:207-210`. The params
  shape beyond `id/header/question` is unverified **(fixture: `codex
  app-server generate-json-schema`)**.
  Codex has no multi-select flag and no notes. Several labels plus Other text
  all go into the `answers` array.
- **MCP elicitation.** `mcpServer/elicitation/request`: a message plus a
  requested JSON schema. Its response is accept / decline / cancel with
  content. amux does not handle it today.

### 1.2 How amux captures a question today

**Claude AskUserQuestion: hooks and the transcript, no pane parsing.**

- The status hook `PreToolUse` with matcher `AskUserQuestion` reports `waiting`
  (`internal/claudecfg/claudecfg.go:613-616`). `PostToolUse` reports
  `running` again (`:621`). Notification hooks report waiting only for the
  blocking types (`:622-626`, `:693-698`). All observation hooks are
  `async` with a 15 s timeout (`:981-994`), so none can answer anything.
- The questions themselves come from the transcript tail:
  - `claudeOpenQuestionsIn` (`internal/daemon/queue.go:208-245`) maps lines
    with `runtimeevents.MapClaudeLine`.
  - An open question is a `tool_call` titled `AskUserQuestion` with no
    `tool_result` and no later `turn_start`.
  - `queue.ParseQuestions` (`internal/queue/queue.go:119-135`) keeps
    `header, question, options{label, description}, multiSelect`. It
    **drops `preview`**, and it has no notion of Other, notes or cancel.
- `claudeQuestionOpen` (`internal/daemon/steer_question.go:33-77`, #316) uses
  the same tail reading as a boolean. It only answers for running or waiting
  sessions, so a crash-dangling call cannot make amux press Esc.
- The #327 waiting state feeds `Supervision.NeedsYou`
  (`harnessproto/session.go:160-164`) and the workgroup buckets
  (`internal/queue/queue.go:287-367`).

**Permissions.**

- Codex structured mode: `Supervisor.onRequest`
  (`internal/codexapp/supervisor.go:1424-1437`) routes `*/requestApproval`
  to `handleApproval`. That function emits `permission_request` with fixed
  options Allow/Deny only (`:1488-1505`). The tracker is
  `internal/codexapp/approvals.go`.
- Codex PTY mode: `runtimeevents/codex.go:476-543` opens permissions from
  rollout events. This covers `exec_approval_request`,
  `apply_patch_approval_request`, `request_user_input` and
  `elicitation_request`.
- Claude: hook-fed diagnostic records (`claudecfg.go:775-806`). They are
  never answer authority.

**Codex user input is not captured.**

- `case method == "tool/requestUserInput"` (`supervisor.go:1428`) emits a
  notice and a raw event and never responds (`:1515-1528`). Codex then waits
  for its own TUI.
- The method name is in doubt. amux's own auth fake sends
  `item/tool/requestUserInput` (`internal/codexauth/fake_test.go:135`). If
  that is the real name, the request falls to `default` and gets -32601,
  which fails the tool. Verify with the generated schema and accept both
  names.
- MCP elicitation and the legacy approval methods also get -32601
  (`supervisor.go:1434-1436`).
- No Codex question reaches the queue: `claudeOpenQuestions` is Claude-only
  (`daemon/queue.go:192-206`).

### 1.3 How a question is represented

| Layer | Type | Ref | Notes |
| --- | --- | --- | --- |
| queue (daemon) | `queue.Entry{ID, Kind, Session, …, Questions []Question, ToolUseID, RequestID, RuntimeGeneration}` with `Question{Header, Question, Options, MultiSelect}` and `Option{Label, Description}` | `internal/queue/queue.go:43-90` | camelCase JSON. The id is `q-`+hash(session, generation, tool_use_id) (`daemon/queue.go:290`) |
| queue verbs | `core.QueryQueue`, `ActionQueueAnswer` (fields `entry`, `text`, `choice`, `decision`), `ActionQueueDismiss` | `internal/core/queue.go:8-33` | `choice` = one line per question, `\|` between labels |
| direct (#337) | `QueueEntry{…, Questions []QueueQuestion{Header, Question, Options, MultiSelect}}` and `QueueAnswerRequest{idem_key, answer, choice[], decision, runtime_generation}` | `internal/direct/queue.go:69-110, 186-202` | snake_case. Strict decode (`readStrict` `:526-545`): unknown keys get 400. `choice` is only allowed for an entry with exactly one question (`:567-576`) |
| direct caps | `QueueCapabilities{read, answer_kinds, triage, acknowledge}` | `internal/direct/queue.go:216-228, 296-299` | |
| harnessproto | **No question type.** `SessionCaps{prompt, interject, cancel, permission, goal}`; `permission_request`/`permission_resolved` events; `VerbPermission` with string fields | `harnessproto/session.go:262-268`, `events.go:78-85`, `harnessproto.go:144-260` | Session-action fields are `map[string]string` on every hop |
| snapshot | Only `state: waiting` and `Supervision.NeedsYou`; no question content | `harnessproto/session.go:24, 160-164` | |
| overseer (#335) | Reads the queue; curates, never answers | `docs/direct-console.md` §4; `internal/overseer/overseer.go` | |
| web | **Nothing.** No question type, event or verb. Direct waiting rows say "answer in its transcript" | `harness/frontend/src/components/rail/RailShell.tsx:568-600` | Codex `requestUserInput` is auto-answered empty by the harness's own pilot (`harness/backend/runtime/codexapp/session.go:684-716`) |
| Android | `Question{question, header, options, multiSelect}` parsed from the transcript `tool_call.raw_input`; `QuestionCard` | `harness-android/core/protocol/.../session/Question.kt`; `app/.../steering/QuestionCard.kt:43-117` | Questions are absent from Needs-you, push and the widget. The card answers with free text |

### 1.4 How an answer is delivered today: typed text

There is no structured channel for questions.

- **Queue `question` answers.** The path is `daemon/queue.go:422-431`:
  1. `queue.FormatAnswer` renders the choices as text: a label, or
     "header: label" lines (`internal/queue/queue.go:137-165`).
  2. `queuePrompt` sends it as `steer prompt` (`daemon/queue.go:461-465`).
  3. `deliverTyped` (`daemon/typed_delivery.go:120-219`) checks for an open
     question and **presses Esc first** (#316, `:146, :170-172`). It then
     waits for the composer with the #334 reader
     (`internal/composer/composer.go:51-61, 212-265`). It refuses a draft,
     types the text, re-reads, and submits only on an exact echo.
- Claude therefore records the question as **declined**, and the answer
  arrives as a new user prompt. Previews, notes and per-question structure
  are all lost. A multi-select answer is a comma list in prose.
- **Android (#73)** sends the same formatted text through the `prompt` or
  `interject` verb. It has no correlation id and no cancel.
- **The TUI queue** gives one-key answers for a lone single-select question
  (`internal/nativetui/queue.go:150-157`). Everything else goes through a
  one-field text form (`:179-189`).
- **The CLI** is `amux queue answer <id> --choice <label> | --text`
  (`cmd/amux/queue.go:24-66`).
- **Permissions** are structured for Codex (`steer.go:334-375`, which sends
  `{"decision":"accept"|"decline"}`). They are keys for PTY sessions
  (`steer.go:163-200`, `agent/keys.go`).
- **Conflicts.** `queueState.answered` (`daemon/queue.go:47-91`) remembers an
  answered id for 10 min, so a second answer gets `answered`. Otherwise it
  gets `asker_gone` (`:385-391`). Two concurrent answers both pass
  `findQueueEntry` and both type, because there is no claim.

### 1.5 Related merged work (reused, not replaced)

- **#316 (`02d67f0`).** Esc-dismiss before a typed steer. v2 keeps it only
  for the explicit "reply instead" fallback when the dialog is drawn and no
  structured channel exists (§4.3).
- **#327 (`a120737`).** `waiting` only when someone is blocked on the user.
  v2 adds no new state; the hold keeps the session `waiting`.
- **#334 (`8a0c776`).** `internal/composer`. v2 adds a dialog reading
  **in the same package on the same cell model** (`rowsOf`, `cell.hint`,
  `cell.cursor`, `isRule`), and reuses `Read` to confirm the composer came
  back after a submit. There is no second screen-scraping stack.
- **#335 (`495da32`) and #337 (`b494c4c`).** The queue, its deterministic
  ids, `asker_gone` / `answered`, direct routes, idempotency
  (`runQueueOp`, `direct/queue.go:770-830`) and audit. v2 extends these
  types; it does not fork them.

---

## 2. Question model v2

### 2.1 Principles

1. **One envelope, one decoder.** The new data lives under a single new key,
   `form`, on the existing queue entry and in a new runtime event. Like
   session enrichments (`harnessproto/enrichment.go:5-27`), the key is one
   word, and **every nested key is snake_case on every wire**. The camelCase
   queue JSON, snake_case direct, the cloud backend and Android can then
   share one decoder.
2. **JSON, not proto.** No amux wire is protobuf. Versioning uses a
   `version` integer that changes only on a breaking change. Additive keys
   are allowed under the same version. A feature string advertises support.
3. **The daemon enforces compatibility.** Old clients cannot be trusted to
   ignore what they do not understand. The daemon therefore decides which
   legacy fields to fill (§2.5) and which legacy answers to accept (§3.5).

### 2.2 Go types

The types live in `harnessproto/question.go`: stdlib only, importable by
harness. `internal/queue/form.go` builds forms, projects them onto the legacy
fields and maps legacy answers. The block below is the design sketch; the
exact code is in `harnessproto/question.go`, which names the types `Form`,
`FormQuestion`, `FormOption`, `FormClaim`, `FormAnswer`, `QuestionAnswer`,
`QuestionRequestPayload`, `QuestionResolvedPayload` and
`ValidateAnswer`/`LegacyAnswerable`.

```go
// FormVersion is the question-form wire version; it changes only for a
// breaking change. Additive keys keep version 2.
const FormVersion = 2

// QuestionPromptsFeature is advertised in register.capabilities.features
// (cloud) and mirrored by direct capabilities.queue.form.
const QuestionPromptsFeature = "question-prompts-v2"

// Form sources.
const (
	SourceClaudeAsk       = "claude_ask_user_question"
	SourceCodexUserInput  = "codex_request_user_input"
	SourceCodexElicit     = "codex_mcp_elicitation" // reserved; phase 3
)

// Delivery modes: how amux will deliver an answer right now.
const (
	DeliveryStructured = "structured" // hook hold or app-server response
	DeliveryKeys       = "keys"       // drive the drawn dialog, verified
	DeliveryPane       = "pane"       // amux cannot deliver; answer in the pane
)

type Form struct {
	Version   int            `json:"version"`             // 2
	Source    string         `json:"source"`
	Questions []FormQuestion `json:"questions"`           // 1..4
	// Cancel: the prompt can be dismissed without an answer. Reply: it can be
	// dismissed with a message to the agent instead ("Chat about this").
	Cancel bool `json:"cancel"`
	Reply  bool `json:"reply"`
	// Delivery is the current mode; it changes (structured → keys) when the
	// Claude hold is released (§4.1), and a client re-renders capabilities.
	Delivery string `json:"delivery"`
	// Deadline is when the prompt auto-continues or the hold releases (Unix
	// ms), when known.
	Deadline int64 `json:"deadline,omitempty"`
	// Claim is set while an accepted answer is being delivered.
	Claim *FormClaim `json:"claim,omitempty"`
}

type FormQuestion struct {
	// ID is stable within the form: Codex's question id; for Claude "q<i>"
	// by position (answers are keyed by question text on delivery).
	ID          string       `json:"id"`
	Header      string       `json:"header,omitempty"`
	Question    string       `json:"question"`
	Options     []FormOption `json:"options,omitempty"` // 0 (free text only) or 2..4 for Claude
	MultiSelect bool         `json:"multi_select,omitempty"`
	// Other: a free-text answer is accepted (Claude: always; Codex: isOther,
	// or a question with no options).
	Other bool `json:"other"`
	// Secret: the free text must not be echoed or logged (Codex isSecret).
	Secret bool `json:"secret,omitempty"`
	// Notes: a per-question note is accepted (Claude structured: true;
	// Claude keys: only for a preview question; Codex: false).
	Notes bool `json:"notes,omitempty"`
}

type FormOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Preview is untrusted markdown shown beside the options; only on a
	// single-select question.
	Preview string `json:"preview,omitempty"`
}

type FormClaim struct {
	By  string `json:"by"`  // "host", "direct:<grant>", "cloud:<provider>"
	At  int64  `json:"at"`  // Unix ms
}

// Entry gains:
//	Form *Form `json:"form,omitempty"`
// Kind stays "question" for every form source (Claude and Codex), so old
// clients bucket it correctly.
```

### 2.3 Wire JSON

The same `form` object appears in four places:

- the queue overview (`amux queue --json`, `QueryQueue`);
- direct `GET /direct/v1/queue` and `/queue/watch` (`QueueEntry.form`);
- the new runtime event `question_request`;
- the cloud relay, passed through raw like enrichments
  (`harness/backend/fleet/snapshot.go:152-199`).

```json
{
  "id": "q-k3v9x2m4pa",
  "kind": "question",
  "state": "open",
  "session_id": "923958-7bead5",
  "title": "Which layout should the settings page use? (+1 more)",
  "questions": [ "…legacy projection, §2.5…" ],
  "form": {
    "version": 2,
    "source": "claude_ask_user_question",
    "cancel": true,
    "reply": true,
    "delivery": "structured",
    "questions": [
      {
        "id": "q0", "header": "Layout",
        "question": "Which layout should the settings page use?",
        "options": [
          {"label": "Tabs (Recommended)", "description": "One tab per area", "preview": "┌Tabs┐…"},
          {"label": "Single page", "description": "Long scroll", "preview": "…"}
        ],
        "other": true, "notes": true
      },
      {
        "id": "q1", "header": "Extras",
        "question": "Which extras do you want?",
        "options": [{"label": "Search"}, {"label": "Dark mode"}, {"label": "Export"}],
        "multi_select": true, "other": true, "notes": true
      }
    ]
  }
}
```

### 2.4 harnessproto additions (the published contract)

- `const QuestionPromptsFeature = "question-prompts-v2"`.
- `SessionCaps.Question bool \`json:"question,omitempty"\``. It is true when
  the daemon can deliver v2 answers for this session's runtime under its
  default configuration:
  - a Claude session with the hold hook enabled;
  - a Codex session in structured mode.

  The cap is static per session, like the other caps. **The open form's
  `delivery` is authoritative per prompt**: it reads `pane` when amux cannot
  deliver that particular prompt. `omitempty` keeps every existing caps
  fixture byte-identical.
- Event `TypeQuestionRequest = "question_request"` with payload
  `{request_id, runtime_generation, form}`. `request_id` is the queue entry
  id (`q-…`), which is deterministic, so the transcript card and the queue
  row are the same object.
- Event `TypeQuestionResolved = "question_resolved"` with payload
  `{request_id, runtime_generation, outcome, via?}`:
  - `outcome` ∈ `answered | cancelled | timed_out | cleared`;
  - `via` ∈ `amux | pane | unknown`.
  - It never carries the answer's content: secrets and audit stay in the
    daemon.
- Verb `VerbAnswer = "answer"` (in `SessionVerbs` and `SteeringVerbs`) with
  fields:
  - `request_id`: the queue entry id;
  - `runtime_generation`: accepted; the entry id already binds the
    generation;
  - `answer` (`FieldAnswer`): the JSON-encoded `FormAnswer` (§3.1).

  The fields stay `map[string]string`, as every hop requires. An old daemon
  returns `ErrUnsupportedVerb`, and the web hides the control
  (`harness/backend/actions/providers.go:184-187`).
  - The daemon serves the verb as a queue answer **bound to the steered
    session**. An entry of another session is `asker_gone`.
  - Only the host (the user, or the cloud relay acting for the user) may
    send it. `access.allowSteer` admits no restricted session principal for
    it, so a coordinator or the overseer can never answer for the user.
  - The relay's claim is recorded as `by: "cloud"`.
- The 409 codes are `asker_gone`, `answered`, `needs_newer_client`,
  `delivery_failed` and `answer_in_pane` (`harnessproto.Code*`). On the
  relay they arrive as the action result's `code`.
- `harnessproto.Version` and `EnrichmentsVersion` do not change. Consumers
  MUST pass the two new event types through as unknown (already required,
  `events.go:60-64`).

Direct gains:

- `QueueCapabilities.Form *FormCapabilities \`json:"form,omitempty"\``, where
  `FormCapabilities{Version int, Sources []string}`. If it is absent, the
  server predates v2.
- `QueueAnswerRequest.Form *FormAnswer \`json:"form,omitempty"\``. An old
  server's strict decoder returns 400 for it, so a client sends it only
  after seeing `capabilities.queue.form.version == 2`. It cannot be combined
  with `answer`, `choice` or `decision`.
- `QueueEntry.Form *Form \`json:"form,omitempty"\``.
- A per-session `SessionCaps.question` (`omitempty`). It is true when the
  grant has `steer` and the row's caps say `question`; for the overseer row
  the grant must also be one that may drive it.
- 409 `needs_newer_client`, `delivery_failed` and `answer_in_pane` join
  `asker_gone` and `answered`.
- The audit record of a form answer never holds its free text. It reads
  `form: q0=0 q1=0,1+other+note` or `form: dismissed[ with a reply]`.
- **Fixtures.**
  - `docs/direct/fixtures/questions-v2/` (with `SHA256SUMS`) is the
    byte-exact contract. It holds entries with forms and their legacy
    projection, the four answer shapes, the conflict errors, the
    capabilities with `queue.form`, session caps, and the two runtime
    events.
  - `queue-v1` is frozen: it describes a server before v2, so existing
    copies stay valid.
  - The harnessproto goldens are `question_request.json`,
    `question_resolved.json`, `session_action_answer.json` and
    `session_caps_question.json`.

The cloud feature: the provider advertises `question-prompts-v2` whenever it
publishes sessions with session verbs enabled.

### 2.5 Backward compatibility rule

The daemon computes `legacy := LegacyProjection(form)`. It fills the
**existing** fields (`questions`, `title`, `body`) only with what an old
client can answer correctly:

| Form property | Legacy `questions` | Legacy answers accepted (§3.5) |
| --- | --- | --- |
| Claude, single/multi-select, no previews | full, as today | `choice` / `text` as today, mapped to a structured answer |
| has previews | options without previews; `body` gains "Option previews are hidden here; open the question in a newer amux client to compare them." | yes (a label is still the user's own pick) |
| Codex user input, no secret | questions with options; no-option questions as free text | yes, mapped per question id |
| any `secret` question | **omitted** | **refused**, 409 `needs_newer_client`, so a secret never goes through a channel that echoes or audits it |
| duplicate question texts (Claude keys answers by text) | omitted | refused, `needs_newer_client` |
| elicitation (phase 3) | omitted | refused |

For an omitted entry, `title` is prefixed with `[open in a newer amux client]`.
The kind stays `question`, so old counts and buckets are unchanged. An old
client therefore either answers correctly or gets a clear refusal. It can
never produce a wrong or partial answer.

---

## 3. Answer model

### 3.1 Types

```go
type FormAnswer struct {
	Version int    `json:"version"` // 2
	Action  string `json:"action"`  // "answer" | "cancel"
	// Message, on cancel, is delivered to the agent as the user's reply
	// ("Chat about this" / "reply instead"). Requires form.reply.
	Message string           `json:"message,omitempty"`
	Answers []QuestionAnswer `json:"answers,omitempty"` // action=answer: one per question
}

type QuestionAnswer struct {
	ID       string  `json:"id"`                 // FormQuestion.ID
	Selected []int   `json:"selected,omitempty"` // option indices
	Other    *string `json:"other,omitempty"`    // free text; nil = not chosen
	Note     string  `json:"note,omitempty"`
}
```

The wire carries **indices**. The entry id is bound to one immutable call
(session, generation, tool_use_id or request id), so an index cannot drift.
Clients show labels. The CLI resolves labels to indices against the entry it
fetched (§5.4).

### 3.2 Validation (daemon, `queue.ValidateAnswer`)

- `action=answer` needs exactly one `QuestionAnswer` per form question, with
  ids matching.
- Single-select: exactly one of {one index} or {non-empty `other`}.
  `other` requires `question.other`.
- Multi-select: ≥1 index (distinct, in range) and/or a non-empty `other`.
- `note` requires `question.notes` and is ≤2000 runes. `other` and
  `message` are ≤ `MaxPromptBytes`.
- `cancel` requires `form.cancel`. A `message` requires `form.reply`.
- `secret` answers are never logged. Audit records `"(secret)"`
  (`direct/queue.go:634` currently records the text).

### 3.3 Conflicts: first answer wins

`queueState` (`daemon/queue.go:50-91`) gains a per-entry claim:
`open → claimed(by, at) → resolved(outcome, via, at)`.

- `ActionQueueAnswer` takes the claim atomically (compare-and-set under
  `q.mu`) **before** delivery. A second answer while claimed or resolved gets
  409 `answered`, naming who claimed it and when. Idempotent retries with the
  same `idem_key` replay the first result (`runQueueOp`).
- If delivery fails before anything reached the agent (keys verification
  failed, the hold went away), the claim is released back to `open`. The
  answerer gets 409 `delivery_failed` with the reason, and the entry
  reappears for everyone. A failure after submit is never retried; that
  case is `cleared` with `via: unknown`.
- Once amux has delivered an answer, the entry leaves every list and refuses
  further answers (`answered`), even before the runtime records it.
  Previously a second answer in that window was delivered again.
- `resolved` is kept for `queueAnsweredTTL` (10 min, as today). A later
  answer gets `answered` (with outcome); after the TTL it gets `asker_gone`.
- The overseer still never answers (`docs/direct-console.md` §4).

### 3.4 Detecting resolution everywhere

A watcher emits `question_resolved`, which drops the entry from every view.
The overseer's `queue/watch` already pushes a fresh snapshot. The resolution
signals are:

| Signal | Outcome |
| --- | --- |
| amux delivered through the hold or app-server, and the tool_result follows | `answered` / `cancelled`, `via: amux` |
| Claude transcript `tool_result` for the tool_use_id when amux did not deliver | `answered`, `via: pane`. If it is an error/rejection: `cancelled` **(fixture: Esc and answer result shapes)** |
| Claude result after `form.deadline` (the user set `askUserQuestionTimeout`) whose content reads as the timeout notice | `timed_out` **(fixture)**. Otherwise `answered` |
| Codex `serverRequest/resolved` for a tracked user-input request (`supervisor.go:1605-1631`) | `answered`, `via: pane` unless amux answered |
| `turn_start` / turn end / runtime restart / archive | `cleared` (generation changes, so the id changes) |

Today's `asker_gone` path (`daemon/queue.go:385-391`) stays the
backstop for anything missed.

### 3.5 Legacy answers on a v2 daemon

Old clients keep working:

- **`choice` on a legacy-answerable entry** (TUI digits, CLI `--choice`,
  direct `choice[]`). Labels are mapped to indices and delivered
  **structurally**. This is strictly better than today's Esc + prompt.
- **`text` on a one-question entry** becomes `other` for that question.
- **`text` on a multi-question entry with no choices** becomes
  `cancel` + `message` (exactly today's semantics: the form is declined and
  the text is the reply).
- **Mixed choices and text on a multi-question entry**: 400, "answer this in
  a newer client or answer every question with --choice". Today's
  `FormatAnswer` guesses here.
- **A typed `prompt`/`interject` steer to a session with a held or open
  question** (Android #73 `questionReply`, any free steer):
  - If the text is exactly the `questionReply`/`FormatAnswer` rendering of a
    valid choice set, it is delivered as that structured answer. This is an
    exact match only.
  - Otherwise it becomes `cancel` + `message`: the question is closed with
    the text as the user's reply.
  - #316's Esc remains the mechanism only when the dialog is drawn and no
    hold exists.

---

## 4. Delivery strategy per harness

### 4.1 Claude: structured hold (primary)

A new blocking hook is installed next to the async status hook:

```json
"PreToolUse": [{ "matcher": "AskUserQuestion",
  "hooks": [{ "type": "command", "command": "<amux> agent question hold", "timeout": 86400 }] }]
```

The status hook keeps reporting `waiting` (`claudecfg.go:616`). `hookCommand`
(`:988-995`) keeps this one synchronous, like the goal hooks.

`amux agent question hold`:

1. Reads the PreToolUse payload (`tool_use_id`, `tool_input`) and registers
   it with the daemon over the agent mailbox. This is the
   `HookPayload` (`claudecfg.go:674-687`), which gains `ToolUseID`
   **(fixture: confirm the field)**.
   - The daemon cross-checks the registration against the transcript's open
     call with that id and input. Until it matches, the entry is not
     listed.
   - This is the same trust level as today. Question text is untrusted
     either way: render it as plain text.
2. Long-polls the daemon in 25 s rounds (`amux agent question wait`), so no
   mailbox call is long-lived. Each round returns one of three outcomes:
   - **answer**: print
     `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{...original tool_input..., "answers":{<question text>: "<labels joined ', '>" | "<other>" | "<labels>, <other>"}, "annotations":{<question text>:{"notes":…, "preview":<chosen option's preview>}}}}}`
     and exit 0. `updatedInput` replaces the whole input, so the original
     `questions` and `metadata` are echoed.
   - **cancel**: print `permissionDecision:"deny"` with
     `permissionDecisionReason`, either "The user dismissed these questions
     without answering." or "The user dismissed these questions and replied:
     <message>". Exit 0.
   - **release**: print nothing and exit 0. Claude then draws its normal
     dialog, and delivery switches to §4.2.
3. Fails open. A daemon that is unreachable, an unknown entry or any error
   means release immediately. A broken amux therefore never costs the user
   the native dialog.

**Release triggers.** These keep the pane fully native:

- Someone types into the pane: `LastHumanInput` moves
  (`engine/local/local.go:472`).
- A TUI client focuses that session's pane. This needs a new
  `ActionQuestionRelease`, sent by `nativetui` on focus.
- The user runs `amux question release <id>`, or presses "answer in pane" in
  any client.
- `form.deadline` passes. That is `askUserQuestionTimeout` when the user set
  one, else never. A 24 h hook timeout bounds the process; the daemon
  releases 60 s before it.
- The daemon restarts.

While held, the pane shows Claude's hook spinner rather than the dialog. The
release rules above are what make that acceptable. This is the main UX
trade-off; see §8.

**Interaction with #316.** While a hold is active, `questionOpen` is true but
no selector is drawn, so Esc would interrupt the turn. `deliverTyped`
therefore resolves a held question by **cancel + message through the hold**,
not Esc (§3.5).

### 4.2 Claude: verified key driving (fallback, after release)

Use this only when the dialog is drawn: the hold was released, or the hook
is missing on an older install.

1. **Read.** `composer.ReadQuestion(runtime, snap) QuestionReading` lives in
   `internal/composer` on the existing `rowsOf`/`cell` model. It returns:
   - `Found`;
   - the tab strip (headers, answered marks), the focused question text,
     option rows (label, checked, focused), the Other row and its text,
     notes text, and `Review` (the submit screen);
   - the side-by-side preview frame, recognized only as a frame.

   It matches the dialog to the form by question text and labels. On any
   mismatch it refuses.
2. **Drive one question at a time.**
   - Single-select option: press its digit. This selects the option and
     advances the tab (changelog 9599, 10654).
   - Multi-select: move the focus to the option (↑/↓ verified per step) and
     toggle it. Re-read until `checked` matches the wanted set.
   - Other: focus the Other row (verified `focused == other`), type the text,
     and re-read until the Other field shows exactly the text
     (`composer.Same`) before any advancing key. Text is never typed unless
     the reader shows the Other or notes field focused.
   - Note (preview questions): open the notes field, then verify, type and
     verify the same way.
3. **Submit.** At `Review`, the reader must show every question answered as
   intended. Then press the submit key. Afterwards `composer.Read` must find
   the composer, and the transcript must show the `tool_result`.
4. **Abort rules.**
   - If a human typed into the pane during driving, or any verification
     times out, stop. Release the claim with `delivery_failed` and say
     "finish in the pane".
   - amux never sends Esc while driving. Esc's vim-mode semantics
     (changelog 2561) make it unsafe.
5. **Cancel.**
   - Plain cancel sends Esc only when the reader shows the option list
     focused, where Esc is not swallowed by a text field.
   - `reply` without a hold is today's #316 path: Esc, verify the composer
     returned, then a typed prompt through `deliverTyped`.

Key-path support ships per feature, gated by fixtures (§7). Until a feature
has a passing fixture, `form.delivery = pane` for entries that need it. The
client says "answer in the pane", and `SessionCaps.Question` stays honest.

### 4.3 Codex: structured app-server (primary; no keys)

Structured mode (coordinators always; opt-in `codex.control`) is a peer
client of the thread (`docs/codex-app-server-supervision.md:3-35`).

- **User input.**
  - Accept `item/tool/requestUserInput` and `tool/requestUserInput`.
  - Decode the full params.
  - Register the request in a tracker modelled on `approvals.go`:
    first-registration, stale and duplicate guards, `serverResolved`, and
    `drainForTurn`.
  - Emit `question_request` with
    `form{source: codex_request_user_input}`.
  - Answer with `{answers:{<id>:{answers:[labels…, other?]}}}`.
  - Cancel answers with empty lists for every id. That is the pilot's
    "no answer" shape **(fixture: what Codex's own TUI sends on Esc)**.
  - `serverRequest/resolved` clears the request: first answer wins between
    amux and the attached TUI.
- **Approvals.**
  - Keep the `permission` kind and add `options[]` to the v2 permission
    payload: `accept`, `acceptForSession` ("Allow for session"), `decline`
    and `cancel`.
  - `steerStructured` (`steer.go:367-375`) accepts a `decision` option id
    from that list. `allow`/`deny` stay aliases, so old clients are
    unchanged.
- **Elicitation (phase 3).** A form with typed fields derived from the
  requested schema (string, number, boolean, enum). It is reserved here and
  listed legacy-omitted.
- **PTY-mode Codex.**
  - User-input entries list with `delivery: pane`. There is no keys path in
    v2; Codex's input overlay has no reader yet. The rollout open/close
    events (`runtimeevents/codex.go:476-497`) still clear the entry.
  - PTY approvals keep `y`/`n` (`agent/keys.go:157-159`).

---

## 5. Client UI spec

Every client renders from `form`, falls back to the legacy fields when `form`
is absent, and gates on `capabilities.queue.form` / `SessionCaps.Question`.

Shared rules:

- Question text, labels, descriptions and previews are untrusted.
- Previews are markdown rendered with sanitizing; the TUI shows plain text.
- Previews and notes are shown only where `form` says so.
- A `secret` field is masked.
- `delivery: pane` shows "Answer this in the agent's pane" with no
  controls, plus "open pane".
- A `claim` by someone else greys out the controls ("being answered on
  Android…").
- A 409 `answered` shows "Already answered" and collapses the card.
- `delivery_failed` shows the reason, and the card stays open.

### 5.1 Common form layout

```
 [☐ Layout] [☐ Extras] [ Review ]                      session · 2m waiting
 Which layout should the settings page use?
 ┌───────────────────────────┐ ┌──────────── preview ────────────┐
 │ ❯ 1. Tabs (Recommended)   │ │ ┌Tabs┐                          │
 │      One tab per area     │ │ │ General │ Account │ …         │
 │   2. Single page          │ │                                  │
 │   3. Other: [__________]  │ │                                  │
 └───────────────────────────┘ └──────────────────────────────────┘
 Note: [________________________]                 (n) note  (x) dismiss
```

- There is a tab per question; the review tab lists each answer and has
  Submit.
- Multi-select rows show `[ ]`/`[✔]`.
- The preview sits beside the list at wide widths (TUI ≥100 cols, web
  ≥768 px) and below it otherwise.
- **Single-question, single-select, no notes:** a tap on an option sends at
  once, as in Claude (changelog 10654) and today's phone card. Everything
  else submits from Review.
- **Dismiss** (`cancel`) and **Reply instead…** (`reply`, a text box) are
  secondary actions.

### 5.2 Native TUI (`internal/nativetui/queue.go`)

- The queue row shows a header chip summary and `+N`.
- `Enter` opens the form view (it replaces `openQueueReplyForm` for
  questions). The keys are:
  - `←/→` or `Tab` switch question;
  - `↑/↓` and `1-9` pick; `Space` toggles in multi-select;
  - `o` edits Other; `n` edits the note;
  - `Enter` on Review submits; `x` dismisses; `m` replies instead;
  - `p` "answer in pane", which releases the hold and focuses the pane.
- Focusing a session pane with a held question sends
  `ActionQuestionRelease` (§4.1).
- Digits `1-9` in the queue list keep their one-key answer only for the
  single-question, single-select case (`queue.go:150-157`).

### 5.3 Console / overseer queue

- The host console and `amux queue` share the TUI view.
- The overseer's guide (`internal/wsops/overseer_guide.go`) learns `form`
  for curation (`suggested_answer` can name option indices) but never calls
  answer.

### 5.4 CLI

```
amux question list [--all] [--json]
amux question show <id> [--json]          # every question, options, previews
amux question answer <id> [--q <n|id|header>] [--select <label|n>]... \
                          [--other <text>] [--note <text>] ...
amux question cancel <id> [--message <text>]
amux question release <id>                # hand the prompt back to the pane
```

- `--q` starts a question group, and the flags after it apply to that
  question. It may be omitted for a one-question form.
- `--select` matches the exact label first, then a 1-based index.
- `--other -` reads stdin. A `secret` question prompts with no echo
  (`--other -` from a pipe is allowed).
- Validation errors name the question and its options.
- Exit codes: 0 delivered, 3 `answered`, 4 `asker_gone`, 5
  `delivery_failed`, 6 `needs_newer_client`.
- `amux queue answer` stays as the legacy alias with its current flags
  (`cmd/amux/queue.go:24-66`). It routes through §3.5.

Example:

```
amux question answer q-k3v9x2m4pa --q Layout --select 1 --note "keep icons" \
                                  --q Extras --select Search --select "Dark mode" --other "keyboard map"
```

### 5.5 Web (harness)

- **Decoder.** `frontend/src/protocol/question.ts` is one tolerant decoder
  in the enrichments style (`protocol/enrichments.ts:243-394`) for both the
  cloud and direct paths.
- **Event types.** Add `question_request`/`question_resolved` to
  `CONTRACT_TYPES` (`session/normalize.ts:20-28`), to `ContractType`
  (`session/events.ts`) and to the backend `vocab`
  (`backend/runtime/runtime.go:68-73`).
- **Reducer.** A `question` entry is keyed by `request_id` and counts toward
  turnState `waiting`.
- **`QuestionView`** in `components/SessionView.tsx`, beside
  `PermissionView` (`:1091-1250`), with the same in-flight guard and TTL.
- **Needs-you.** `rail/RailShell.tsx` lists question entries from
  `/direct/v1/queue` or the relayed events instead of "answer in its
  transcript" (`:568-600`).
- **Answer paths.**
  - Direct: `POST /direct/v1/queue/{id}/answer {idem_key, form}`.
  - Cloud: `daemon_session.answer` with `{session_id, request_id,
    runtime_generation, answer:<json string>}`. This needs a new
    `steeringWireFields` and `validateSteering` case
    (`backend/actions/providers.go:258-304`).
- **Push kind `question`.** The data has no question content (it is
  content-free, like the #313 contract). Tapping it opens the card.

### 5.6 Android (harness-android)

- `:core:protocol` `session/Question.kt` gains `Form`, `FormQuestion`,
  `FormOption`, `FormAnswer` and `QuestionAnswer` (all optional, lenient,
  under `ProtocolJson`). It also gains the event payloads and a reducer
  `Entry.Question` keyed by `request_id`. Today's `openQuestions`
  (transcript `raw_input`) remains the legacy fallback.
- **`QuestionCard` v2** (`steering/QuestionCard.kt`) has:
  - a pager per question with header chips;
  - a radio or checkbox list with descriptions;
  - an "Other" text field (masked if `secret`);
  - a note field;
  - preview below the options (a bottom sheet at large font scale);
  - a review step, Dismiss and Reply instead.

  Its test tags extend the current ones (`question-card`,
  `question-option-{qi}-{oi}`, `question-other-{qi}`,
  `question-note-{qi}`, `question-submit`, `question-dismiss`).
- **Enabled state.** The card is enabled by `SessionCaps.question` and the
  form's `delivery`, not by `turnOpen` (`SteeredSessionRoute.kt:51-83`).
- **Needs-you, push and widget.** Add `QueueItem.Question`, answerable inline
  for one-tap forms; otherwise it opens the card. It replaces
  `MachineWaiting` when a form is known. Add push kind `question`.
- **Direct.** Add the `answer` verb to `SESSION_VERB_FIELDS`, or use the queue
  route (`DirectApi.kt`). The JSON is carried as a string field.

---

## 6. Work split, dependencies and merge order

### 6.1 agent-multiplexer

| PR | Content | Depends on |
| --- | --- | --- |
| **A1 model + wire** | `queue.Form`/`FormAnswer`, `ValidateAnswer`, `LegacyProjection`, entry claim state; direct `form` caps/request/response; harnessproto feature, `SessionCaps.Question`, events, `VerbAnswer`; fixtures `docs/direct/fixtures/questions-v2/*.json` + golden tests; legacy mapping §3.5 (still delivered by today's typed path) | — |
| **A2 Claude hold** | `amux agent question hold/wait`, hook install, release triggers + `ActionQuestionRelease`, structured delivery, resolution watcher (`question_resolved`), #316 interaction | A1 |
| **A3 Codex structured** | method-name fix, full params, user-input tracker + answer/cancel, `acceptForSession`/`cancel` approval options | A1 |
| **A4 TUI + CLI** | form view, `amux question …` | A1 (A2/A3 for live delivery) |
| **A5 key fallback** | `composer.ReadQuestion`, verified driving per feature, captured Claude pane fixtures | A1, A2 |
| A6 (phase 3) | Codex MCP elicitation | A3 |

### 6.2 harness

| PR | Content | Depends on |
| --- | --- | --- |
| **H1 relay + contract** | bump harnessproto pin to A1; `daemon_session.answer` relay + validation; caps pass-through (`resolveDaemonCaps`, `snapshot.go:653-664`); vocab + push kind `question`; copy fixtures into `frontend/src/direct/fixtures/questions-v2` (SHA256SUMS) | A1 merged |
| **H2 web UI** | decoder, reducer, `QuestionView`, Needs-you rows, direct/cloud answer | H1 |
| H3 | harness's own Codex pilot (`backend/runtime/codexapp/session.go:684-716`) emits `question_request` instead of the empty auto-answer | H1 |

### 6.3 harness-android

| PR | Content | Depends on |
| --- | --- | --- |
| **D1 protocol** | models, payloads, reducer, fixture copies (`core/network/src/test/resources/direct/questions-v2`) | A1 fixtures |
| **D2 UI** | `QuestionCard` v2, Needs-you/push/widget, direct + cloud answer | D1; cloud path needs H1 deployed |

### 6.4 Merge order (old clients keep working at every step)

1. **A1.** This is additive. Legacy fields are unchanged except for the §2.5
   omissions, which are new entry types anyway: Codex secret and duplicate
   texts never appeared before. Legacy answers still work.
2. **A3, then A2.** Delivery becomes structured behind the same verbs. Old
   clients' `choice` answers improve silently. A2 also changes what a typed
   steer does to a held question (§3.5); old Android keeps working through
   the exact-match rule.
3. **A4**, then **A5** when its fixtures pass.
4. **H1**, which needs the A1 pin. Then **H2**. The web hides v2 UI until it
   sees the feature or the cap.
5. **D1**, then **D2**. Android gates on caps, so it can ship against an old
   daemon and old cloud.

There is no step at which a client needs a newer peer. Every new client
falls back to the legacy view when the cap is absent. Every new daemon serves
legacy readers through the projection.

---

## 7. Test plan

**Protocol round-trip.** One test per feature, in each repo, against the
shared fixtures `docs/direct/fixtures/questions-v2/`:

- single-select; multi-select; Other (single and combined with multi);
  previews; notes; 4 questions; cancel; cancel + message;
  Codex free-text question; Codex secret; unknown future keys
  (`form.questions[].foo`, `form.version: 3` gives the legacy view);
- each entry fixture is decoded by Go (direct, harnessproto golden), TS
  (`wire.test.ts`) and Kotlin (`DirectWorkflowFixturesTest`-style), then
  re-encoded byte-equal where the language allows;
- answer fixtures are validated by `queue.ValidateAnswer`, accepted and
  rejected cases both;
- legacy projection tables (§2.5) and legacy answer mapping (§3.5) are
  table tests in `internal/queue`;
- claim and conflict: two concurrent answers give exactly one delivery and
  one `answered`. A failed delivery releases the claim. An idem replay
  returns the first result.

**Claude delivery.**

- **Capture fixtures on a host** with a real Claude Code 2.1.330 and record
  them under `internal/daemon/testdata/claude-ask/`:
  - the PreToolUse payload (confirm `tool_use_id`);
  - transcript `tool_use`/`tool_result` lines for answer, multi-select,
    Other, notes with preview, Esc cancel, hook deny, hook allow+answers,
    and `askUserQuestionTimeout` expiry;
  - whether PermissionRequest and Notification fire for AskUserQuestion,
    and of which type;
  - hook timeout behaviour.
- Hold tests use a fake mailbox:
  - answer gives exact `updatedInput` JSON (answers keyed by text,
    annotations, original fields echoed);
  - cancel gives a deny reason;
  - each release trigger gives empty output;
  - daemon down gives an immediate release.
- One end-to-end smoke (opt-in, like the existing `*_smoke_test.go`) runs
  real Claude with the hook and confirms that the model receives the
  answers.
- **Pane fixtures.** Capture vterm snapshots (the format `composer_test.go`
  uses) of the dialog in each state:
  - tabs, focus on each option, multi-select checked, Other focused and
    typed, notes, preview side-by-side at 120 and 80 cols, Review, vim
    mode;
  - each at a narrow and a wide width.

  `ReadQuestion` is table-tested on them. Driving is tested against a
  scripted fake pane that replays snapshot transitions, including mismatch,
  human-typed-mid-drive and verification timeout. Each must abort without
  submitting.

**Codex delivery.**

- Extend `fakeserver_test.go`:
  - both method names;
  - full params;
  - the answer/cancel response shapes;
  - `serverRequest/resolved` racing an amux answer (first wins; the second
    gets `answered`);
  - turn drain;
  - approval `acceptForSession`.
- Capture a real schema (`codex app-server generate-json-schema`) into
  `internal/codexapp/testdata/` and add a contract test that pins the
  method names and fields.

**Clients.**

- **TUI.** `teatest`-style golden renders of the form view (each fixture,
  80 and 120 cols) and key-handling tests (digit one-tap, multi toggle,
  Other/note editing, review submit).
- **CLI.** Flag parsing (label vs index, groups, `-` stdin), exit codes,
  `--json`.
- **Web.**
  - Vitest + Testing Library for `QuestionView` (each fixture; one-tap;
    review; secret masked; claim greyed out; `delivery: pane`; 409
    handling).
  - Decoder tests in the `enrichments.test.ts` style, including unknown
    keys and version.
  - `DaemonPane.test.tsx` cases for the cloud relay answer.
- **Android.**
  - Robolectric Compose tests in `QuestionCardTest` (each fixture, font
    scale `SCALE_LARGEST` with `assertNotClipped`).
  - `NeedsYouScreenTest` question rows.
  - Push kind tests.
  - Roborazzi evidence shots.
  - A `/question` endpoint in `e2e/direct-harness` for device acceptance.

---

## 8. Open risks

1. **The hold hides the native dialog** until release. Mitigation: release on
   any pane input or TUI focus, plus "answer in pane" everywhere. If users
   still dislike it, there is a per-user setting
   `questions.claude.hold = auto|off`. `off` means keys-only and is
   honestly capped.
2. **Undocumented Claude behaviour** that the design depends on:
   - `tool_use_id` in the PreToolUse payload;
   - hook timeout semantics;
   - tool_result shapes for Esc and timeout;
   - whether a blocked PreToolUse hook survives Esc/Ctrl+C in the pane
     (expected: the turn aborts, the hook is killed, and the transcript
     shows the interruption as `cancelled`).

   All are fixture-gated (§7) before A2 merges.
3. **Duplicate question texts** cannot be answered structurally, because
   Claude keys answers by text. They are rare. They get keys or pane
   delivery and the legacy omission.
4. **Codex method name.** If the real method is `item/tool/requestUserInput`,
   amux today **fails** such tool calls with -32601. A3 fixes this
   independently of v2 and should land early.
5. **The key fallback is brittle across Claude releases.** It is
   fixture-gated per feature, and it refuses on any mismatch. Answers
   degrade to "answer in pane", never to a wrong answer.
6. **Claude validates `updatedInput`** (changelog 2657). Input it rejects
   denies the call, so a malformed answer would cancel the question. The
   daemon validates the answer (§3.2) and builds the payload from the
   original input only.
7. **Annotations are not in the official hook docs**; they are in the tool
   schema. If Claude drops them, notes are lost silently. The fixture must
   confirm that the model sees them.

---

## Contract changelog

Each entry names the agent-multiplexer branch commit that carries it.
Members pin to these.

- **Layer 1, model and wire** (branch `amux/macstrosity/923958-7bead5`):
  - `harnessproto/question.go`: Form/answer types, `ValidateAnswer`,
    `LegacyAnswerable`, the `question-prompts-v2` feature, and codes.
  - `VerbAnswer`/`FieldAnswer`, `TypeQuestionRequest`/`TypeQuestionResolved`
    and `SessionCaps.Question` (`omitempty`).
  - direct `queue.form` caps, `QueueEntry.form`, `QueueAnswerRequest.form`,
    `SessionCaps.question` and the new 409 codes.
  - fixtures `docs/direct/fixtures/questions-v2/`.
  - Deviations from the approved design:
    - the cap means "deliverable under the default configuration", and the
      form's `delivery` is authoritative (§2.4);
    - `answer_in_pane` is added;
    - `runtime_generation` on `VerbAnswer` is accepted but not required;
    - partial legacy labels on a several-question entry are refused (§3.5).

  At this layer every Claude form reports `delivery: pane` (no channel yet),
  and legacy answers keep the typed reply.
- **Relay refusal codes** (it-fadb4071, from e4b5c5): `HarnessMsg.Code`
  (`json:"code,omitempty"`) on a failed `session-result`.
  - The provider sets it from the daemon's `RejectedError.Code`, so a
    relayed `answer` refusal arrives as one of `asker_gone`, `answered`,
    `needs_newer_client`, `delivery_failed` or `answer_in_pane` without
    parsing `error`.
  - Any other daemon refusal that carries a code relays it too. A result
    with no code omits the key.
  - Golden: `harnessproto/testdata/session_result_code.json`.
- **Codex delivery** (item 1):
  - **User input.** Structured Codex `request-user-input` now appears in the
    queue as a `question` entry with `form.source: codex_request_user_input`
    and `delivery: structured`.
    - The entry carries `requestId` (the JSON-RPC id), and its id is
      `q-…` over (session, generation, "codex-input", request id).
    - Answers go back on the request's JSON-RPC id.
    - A secret question keeps the entry from legacy clients (title prefix,
      legacy answer gives `needs_newer_client`).
    - Both method names, `item/tool/requestUserInput` and
      `tool/requestUserInput`, are accepted. Before this, the `item/…`
      spelling was refused with -32601.
  - **Approvals.**
    - New decision `allow_session` (`harnessproto.DecisionAllowSession`).
      Structured Codex `permission_request` options gain
      `{"id":"acceptForSession","label":"Allow for session","kind":"allow_session"}`.
    - `permission` / queue answers / direct `decision` accept
      `allow_session` for structured Codex; it is sent as
      `acceptForSession`.
    - PTY sessions still refuse it (no key binding). Clients offer it only
      when the request's options list that kind.
    - harness `validateSteering` must admit it to relay it.
- **Claude question hold** (item 2):
  - **Install.** `amux agent question hold` is installed as a blocking
    PreToolUse hook (matcher `AskUserQuestion`, timeout 1800 s) next to the
    async status hook.
  - **While held:**
    - the entry's `form.delivery` is `structured` and `form.deadline` is set
      (Unix ms);
    - a v2 or legacy answer resolves the hook with
      `permissionDecision: allow` plus `updatedInput`;
    - `updatedInput` is the original input with `answers` keyed by question
      text (labels and free text joined with ", ") and `annotations`
      (`notes`, plus the chosen `preview` on a single-select).
    - A dismissal resolves it with `deny` and a reason ("The user dismissed
      these questions [and replied: …]").
  - **Released** (Claude draws its own dialog; the entry turns
    `delivery: pane`) on any of:
    - human input in the pane;
    - host `queue-release` / `amux question release`;
    - direct `POST /direct/v1/queue/{id}/release` (`{idem_key}`; steer
      permission; `capabilities.queue.form.release: true`);
    - the deadline: below the 1800 s hook timeout and the user's
      `askUserQuestionTimeout`, each by `min(60s, timeout/4)`;
    - `amux config set questions.claude_hold off`;
    - an input over 48 KiB or one that is not a question.
  - **Fails open.** The hook prints nothing on any error and never exits 2.
  - **Routing.**
    - A held entry appears in the queue even before the transcript shows the
      call. It uses the same id as the transcript-derived entry.
    - A person's typed prompt or interject to a session with a held question
      answers it when it exactly matches the legacy label rendering;
      otherwise it dismisses it with the text as the reply. No Esc is sent,
      since no dialog is drawn.
    - amux's own automated text is refused while a question is held.
  - **Caps.** `SessionCaps.question` is true for every Claude session and
    for Codex coordinators.
  - The session RPC adds `question-hold` (a self-only query). It is internal
    to the hook.
