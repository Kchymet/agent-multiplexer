package nativetui

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "<esc>":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "<bs>":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "<sp>":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// drive opens a fresh repo-agent form and applies the keys, returning the first
// (Prompt) text field's value. The form opens in normal mode, so editing tests
// press "i" first to start typing.
func drive(keys ...string) string {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	for _, k := range keys {
		m.handleForm(key(k))
	}
	return m.form.fields[0].value
}

func TestVimFormEditing(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"insert + backspace", []string{"i", "abc", "<bs>"}, "ab"},
		{"append with A", []string{"i", "ab", "<esc>", "A", "cd"}, "abcd"},
		{"replace with r", []string{"i", "cat", "<esc>", "0", "r", "b"}, "bat"},
		{"delete char x", []string{"i", "abc", "<esc>", "0", "x"}, "bc"},
		{"delete line dd", []string{"i", "abc", "<esc>", "d", "d"}, ""},
		{"word + D", []string{"i", "foo bar", "<esc>", "0", "w", "D"}, "foo "},
		{"insert at start I", []string{"i", "bar", "<esc>", "I", "X"}, "Xbar"},
		{"change word cw", []string{"i", "foo bar", "<esc>", "0", "c", "w", "baz "}, "baz bar"},
		{"space inserts", []string{"i", "a", "<sp>", "b"}, "a b"},
	}
	for _, tc := range cases {
		if got := drive(tc.keys...); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A freshly opened form starts in normal mode: the first field is selected for
// navigation, not entry. The user opts into typing with i/a/A/I.
func TestFormOpensInNormalMode(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	if m.form.insert {
		t.Fatal("form should open in normal mode, not insert")
	}
	// A bare letter is a motion/command, not text.
	m.handleForm(key("x"))
	if got := m.form.fields[0].value; got != "" {
		t.Fatalf("normal-mode key should not type, got %q", got)
	}
}

// The new-agent form offers a model selector defaulting to opus, with vim
// motions (j to reach it, l to cycle) driving the selection.
func TestModelSelectorDefaultsOpusAndCycles(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")

	var modelField *formField
	for _, f := range m.form.fields {
		if f.key == "model" {
			modelField = f
		}
	}
	if modelField == nil {
		t.Fatal("no model field")
	}
	if !modelField.isSelect() {
		t.Fatal("model field should be a selector")
	}
	if got := modelField.value; got != agent.HarnessFor("claude").DefaultModel() {
		t.Fatalf("default model: got %q, want %q", got, agent.HarnessFor("claude").DefaultModel())
	}

	// Navigate from the prompt field down to the model selector with j, then
	// cycle forward with l. opus -> sonnet.
	m.handleForm(key("j")) // prompt -> mode
	m.handleForm(key("j")) // mode -> agent (Harness)
	m.handleForm(key("j")) // agent -> model
	if m.form.active() != modelField {
		t.Fatal("j navigation did not land on the model field")
	}
	m.handleForm(key("l"))
	if got := modelField.value; got != agent.HarnessFor("claude").Models()[1] {
		t.Fatalf("after cycling: got %q, want %q", got, agent.HarnessFor("claude").Models()[1])
	}
}

// Member creation offers a Harness selector defaulting to claude.
func TestFormsCarryHarnessField(t *testing.T) {
	cases := []struct {
		name string
		open func(*model)
	}{
		{"new-repo-agent", func(m *model) { m.openNewRepoAgentForm("repo", "Repo") }},
		{"add-agent", func(m *model) { m.openAddAgentForm("root", "Root") }},
	}
	for _, tc := range cases {
		m := &model{}
		tc.open(m)
		f := m.form.field("agent")
		if f == nil {
			t.Fatalf("%s: no harness field", tc.name)
		}
		if !f.isSelect() {
			t.Fatalf("%s: harness field should be a selector", tc.name)
		}
		if got := f.value; got != agent.DefaultKind() {
			t.Fatalf("%s: default harness got %q, want claude", tc.name, got)
		}
		if !slices.Equal(f.options, agent.Kinds()) {
			t.Fatalf("%s: harness options got %v, want %v", tc.name, f.options, agent.Kinds())
		}
	}
}

func TestNewWorkgroupUntouchedReposAreOmitted(t *testing.T) {
	m := &model{}
	m.openNewWorkgroupForm()
	fields := formFieldsForSubmit(m.form)
	if _, explicit := fields["repos"]; explicit {
		t.Fatalf("untouched new-workgroup repos must be omitted, got %q", fields["repos"])
	}
	if fields[wsops.FieldCoordinator] != agent.GoalRuntime {
		t.Fatalf("coordinator default was lost: %q", fields[wsops.FieldCoordinator])
	}

	// Blank repos on other actions retain their established exact-empty meaning;
	// only host workgroup creation has the deliberate omitted-grant default.
	m.openAddAgentForm("root", "Root")
	fields = formFieldsForSubmit(m.form)
	if value, explicit := fields["repos"]; !explicit || value != "" {
		t.Fatalf("add-agent blank repos = %q, explicit=%v; want explicit empty", value, explicit)
	}
}

// Cycling the Harness selector reconciles the dependent Model selector: codex's
// models replace claude's, and the now-invalid opus default resets to codex's
// default. Cycling back to claude restores the claude models (and default).
func TestHarnessCyclesModelOptions(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")

	// prompt -> mode -> agent (Harness), then cycle claude -> codex.
	m.handleForm(key("j"))
	m.handleForm(key("j"))
	if m.form.active() != m.form.field("agent") {
		t.Fatal("j navigation did not land on the harness field")
	}
	m.handleForm(key("l"))

	model := m.form.field("model")
	if !slices.Equal(model.options, agent.HarnessFor("codex").Models()) {
		t.Fatalf("model options after codex: got %v, want %v", model.options, agent.HarnessFor("codex").Models())
	}
	if got := model.value; got != agent.HarnessFor("codex").DefaultModel() {
		t.Fatalf("invalid model should reset: got %q, want %q", got, agent.HarnessFor("codex").DefaultModel())
	}

	// Back to claude restores the claude list and its default.
	m.handleForm(key("h"))
	if !slices.Equal(model.options, agent.HarnessFor("claude").Models()) {
		t.Fatalf("model options after claude: got %v, want %v", model.options, agent.HarnessFor("claude").Models())
	}
	if got := model.value; got != agent.HarnessFor("claude").DefaultModel() {
		t.Fatalf("model should reset to claude default: got %q, want %q", got, agent.HarnessFor("claude").DefaultModel())
	}
}

// The agent-creating form serializes both the harness and model picks into the
// action fields the daemon consumes.
func TestAgentFormSubmitsHarnessAndModel(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	v := m.form.values()
	if got := v["agent"]; got != agent.DefaultKind() {
		t.Fatalf("agent field: got %q, want claude", got)
	}
	if got := v["model"]; got != agent.HarnessFor("claude").DefaultModel() {
		t.Fatalf("model field: got %q, want %q", got, agent.HarnessFor("claude").DefaultModel())
	}
}

// The rename form carries a single name field and dispatches the "rename"
// action (with the typed name) for the targeted session, leaving its id alone.
func TestRenameFormSubmit(t *testing.T) {
	m := &model{}
	m.openRenameForm("a1b2c3", "old-label")
	if got := m.form.action; got != "rename" {
		t.Fatalf("action: got %q, want rename", got)
	}
	if got := m.form.id; got != "a1b2c3" {
		t.Fatalf("id: got %q, want a1b2c3", got)
	}
	for _, k := range []string{"i", "new", "<sp>", "name"} {
		m.handleForm(key(k))
	}
	if got := m.form.fields[0].value; got != "new name" {
		t.Fatalf("name field: got %q, want %q", got, "new name")
	}
	if got := m.form.values()["name"]; got != "new name" {
		t.Fatalf("submitted name: got %q, want %q", got, "new name")
	}
}

// Esc leaves insert mode; subsequent letters are commands, not text.
func TestVimNormalModeNotTyped(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	for _, k := range []string{"i", "hi", "<esc>", "x", "x", "x"} { // x deletes, doesn't type
		m.handleForm(key(k))
	}
	if got := m.form.fields[0].value; got != "" {
		t.Fatalf("normal-mode x should delete, got %q", got)
	}
	if m.form.insert {
		t.Fatal("should be in normal mode after Esc")
	}
}

// The creation form exposes exactly two coordinator controls and sends their
// values under coordinator keys, never the member harness/model keys.
func TestWorkgroupCoordinatorControls(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	m := &model{w: 120, h: 40}
	m.openNewWorkgroupForm()
	fs := m.form
	for _, key := range []string{"linear", "mode", "agent", "model"} {
		if fs.field(key) != nil {
			t.Errorf("unexpected creation control %q", key)
		}
	}
	if len(fs.fields) != 5 {
		t.Fatalf("creation has %d controls, want five", len(fs.fields))
	}
	if !strings.Contains(m.renderForm(), "Repos (optional") {
		t.Fatal("repositories must be visibly optional")
	}
	harness, model := fs.field(wsops.FieldCoordinator), fs.field(wsops.FieldCoordinatorModel)
	if harness == nil || model == nil {
		t.Fatal("missing coordinator controls")
	}
	if harness.value != agent.GoalRuntime || !slices.Equal(harness.options, agent.Kinds()) {
		t.Fatalf("coordinator harness = %+v", harness)
	}
	// Drive real key handling through every harness, including back to default.
	fs.cursor = slices.Index(fs.fields, harness)
	for range agent.Kinds() {
		m.handleForm(key("l"))
		h := agent.HarnessFor(harness.value)
		if !slices.Equal(model.options, h.Models()) || model.value != h.DefaultModel() {
			t.Fatalf("%s model after switching: %+v", harness.value, model)
		}
		m.handleForm(key("j"))
		m.handleForm(key("l"))
		fields := formFieldsForSubmit(fs)
		if fields[wsops.FieldCoordinator] != harness.value || fields[wsops.FieldCoordinatorModel] != model.value {
			t.Fatalf("coordinator choices lost: %#v", fields)
		}
		m.handleForm(key("k"))
	}
	fs.field("repos").value = "api,web"
	if got := formFieldsForSubmit(fs)["repos"]; got != "api,web" {
		t.Fatalf("selected repos = %q", got)
	}
	fs.field("repos").value = "  "
	if _, ok := formFieldsForSubmit(fs)["repos"]; ok {
		t.Fatal("cleared picker must omit repositories")
	}
}

// Exercise the actual form payload through lifecycle persistence, without any
// tracked repository or member. A selected model must reach the coordinator.
func TestWorkgroupFormCreatesWithoutRepos(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_JAIL", "off")
	for _, kind := range agent.Kinds() {
		t.Run(kind, func(t *testing.T) {
			m := &model{}
			m.openNewWorkgroupForm()
			fs := m.form
			fs.field("name").value = "No repositories"
			fs.field("prompt").value = "Plan the next task"
			harness := fs.field(wsops.FieldCoordinator)
			harness.value = kind
			fs.syncDependents(harness)
			model := fs.field(wsops.FieldCoordinatorModel)
			if model.isSelect() {
				model.cycle(true)
			} else {
				model.value = "provider/custom-model"
			}
			id, err := wsops.ApplyResult(context.Background(), core.Action{Action: fs.action, Fields: formFieldsForSubmit(fs)})
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			root, ok, err := db.GetSession(id)
			if err != nil || !ok {
				t.Fatalf("created coordinator missing: %v", err)
			}
			if root.Agent != kind || root.Model != model.value || root.Prompt != "Plan the next task" || root.Name != "No repositories" {
				t.Fatalf("coordinator did not retain form values: %+v", root)
			}
			children, err := db.Children(id)
			if err != nil || len(children) != 0 {
				t.Fatalf("repo-less workgroup children = %v, err = %v", children, err)
			}
		})
	}
}

func TestWorkgroupFormReadableAtOrdinaryWidths(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, width := range []int{80, 100, 120} {
		m := &model{w: width, h: 24}
		m.openNewWorkgroupForm()
		for _, kind := range []string{"codex", "claude"} {
			harness := m.form.field(wsops.FieldCoordinator)
			harness.value = kind
			m.form.syncDependents(harness)
			for _, field := range m.form.fields {
				m.form.cursor = slices.Index(m.form.fields, field)
				out := plain(m.renderForm())
				for _, want := range []string{"Coordinator harness:", "Coordinator model:", "Repos (optional):", kind, agent.HarnessFor(kind).DefaultModel(), "Create workgroup"} {
					if !strings.Contains(out, want) {
						t.Fatalf("width %d, active %s: missing %q:\n%s", width, field.key, want, out)
					}
				}
			}
		}
	}
}
