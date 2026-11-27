# Question prompts: schema, controls and answers

An agent's question reaches the user through the question queue
(`internal/queue`, [direct-console.md §4](direct-console.md)). This doc covers
how every client shows a question and collects its answer, so the TUI, the
CLI and the phone draw the same control for the same question.

## 1. Rules for every client

- **Show the whole question.** The header, the full question text, every
  option label with its description and preview: wrap it to the width, scroll
  if needed, and never cut it to one line or a fixed number of rows. Agent
  text is untrusted: render it as plain text.
- **Draw the control the schema names** (§2), one per question when an ask
  has several.
- **Send per-question answers** (§3). The daemon validates them against the
  live question and formats the reply.

## 2. Schema

Each queue entry carries `input`:

| `input` | Kinds | Control |
| --- | --- | --- |
| `questions` | `question` | one control per entry in `questions[]` |
| `decision` | `permission` | allow / deny |
| `text` | `goal_input`, `decision_needed` | a typed answer under the entry's `body` |

Each question in `questions[]` (derived by `queue.Question.Normalize` when it
is read from the `AskUserQuestion` call: labels trimmed, empty and repeated
labels dropped) carries:

| Field | Meaning |
| --- | --- |
| `header` | short chip, optional |
| `question` | the full question text |
| `options[]` | `{label, description?, preview?}`; `preview` is plain text (a mockup or snippet) |
| `input` | `single_select` (pick one), `multi_select` (toggle any, then send) or `text` (no options) |
| `allowOther` / `allow_other` | free text may replace a single_select's pick or be added to a multi_select's. Claude Code's selector always has "Other", so this is true whenever there are options |
| `multiSelect` / `multi_select` | the raw flag, kept for older clients |

The daemon's own JSON (`amux inbox --json`, `core.QueryQueue`) uses camelCase;
the direct API (`/direct/v1/queue`, the phone) uses snake_case. See the
`queue-v1` fixtures in `docs/direct/fixtures/queue-v1/`.

## 3. Answers

A question entry is answered with `answers`: one `{choice, other}` per
question, in `questions[]` order.

- `choice`: option labels. At most one unless `multi_select`.
- `other`: typed text — a `text` question's answer, or "Other" on a question
  with `allowOther`. A single_select takes a choice **or** other text, not
  both.
- An empty answer (or a missing tail) leaves its question unanswered; at least
  one question must be answered.

Transports: `-f answers=<json>` on `queue-answer`
(`core.QueueAnswersField`), `amux inbox answer <id> --answers <json>`, or
`answers` on `POST /direct/v1/queue/{id}/answer`. The older `choice` / `text`
fields still work.

**What the agent receives.** amux answers a Claude `AskUserQuestion` by
dismissing the selector and typing the reply as the user's prompt (#316,
[steering.md](steering.md)), so the reply is text. `queue.FormatAnswers` makes
it the same text the phone's card sends (harness-android#73): a lone question
is answered with its value; several are answered one per line as
`header: value`, with the question text as the key when the header is missing
or shared. A value is the chosen labels in option order, then any other text,
joined by `, `. For example:

    Scope: api, web, docs too
    Ship: yes

## 4. Surfaces

- **TUI** (`internal/nativetui/answer.go`): Enter or `r` on a queue entry
  opens the answer view over the queue panel. Single-selects are radio lists
  `( )`/`(•)`, multi-selects checkboxes `[ ]`/`[✔]`, with an Other row under
  the options and an Answer row for text. ↑/↓/Tab move, Space toggles, 1–9
  pick in the focused question, Enter on a single-select picks and moves to
  the next question, typing on a text row edits it (Ctrl+J newline in a text
  answer, Ctrl+U clear), Ctrl+S or Enter on "Send answer" sends, Esc cancels.
  The view wraps everything to the pane and scrolls to keep the focused
  control in view. A lone single-select can still be answered from the list
  with 1–9; permissions with y/n.
- **CLI**: `amux inbox` prints every question whole, wrapped to the
  terminal. `amux inbox answer <id>` with no other arguments, in a terminal,
  asks each question in turn: an option number (or your own text) for a
  single-select, several numbers (plus any words of your own) for a
  multi-select, a line for a text question; an empty line skips one.
- **Direct API / phone**: the schema above; answer with `answers`.

## 5. Not covered

- Codex `request_user_input` is left open for an attached Codex client
  (`codexapp.handleUserInput`) and does not enter the queue; a Codex
  coordinator asks in plain text instead.
- Claude MCP elicitation dialogs are not read into the queue.
