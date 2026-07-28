package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
)

// -- target -----------------------------------------------------------------

type targetScreen struct {
	menu *Menu
}

func newTargetScreen() *targetScreen {
	return &targetScreen{menu: NewMenu([]MenuItem{
		{Value: "vip", Label: "WordPress VIP environment",
			Desc: "runs `vip @app.env -- wp vip-search ...` through VIP-CLI"},
		{Value: "wp", Label: "Direct wp-cli",
			Desc: "runs `wp vip-search ...` against a site where wp already works"},
	})}
}

func (s *targetScreen) Title() string { return "target" }
func (s *targetScreen) Init() tea.Cmd { return nil }

func (s *targetScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if key.String() == "q" {
		return s, tea.Quit
	}
	if !s.menu.Update(key) {
		return s, nil
	}
	if s.menu.Selected().Value == "vip" {
		return s, push(newEnvInputScreen())
	}
	return s, push(newWPInputScreen())
}

func (s *targetScreen) View() string {
	return styleHeading.Render("How do you reach WordPress?") + "\n\n" +
		s.menu.View() +
		styleHelp.Render("↑/↓ move · enter select · q quit")
}

func newEnvInputScreen() *inputScreen {
	in := newInputScreen("environment", "Which environment?",
		"e.g. @example-app.production — from `vip app list`. Also reads $VIP_APP_ENV.",
		os.Getenv("VIP_APP_ENV"))
	in.validate = func(v string) string {
		if v == "" {
			return "an environment is required — this tool will not guess where to index"
		}
		return ""
	}
	in.submit = func(v string) tea.Cmd {
		sess := newSession()
		sess.target.AppEnv = v
		return push(newActionScreen(sess))
	}
	return in
}

func newWPInputScreen() *inputScreen {
	in := newInputScreen("wp-cli", "How is wp invoked?",
		"plain `wp`, or with extra args: `wp --path=/srv/site`", "wp")
	in.validate = func(v string) string {
		if v == "" {
			return "a wp command is required"
		}
		return ""
	}
	in.submit = func(v string) tea.Cmd {
		sess := newSession()
		sess.target.WPCommand = v
		return push(newActionScreen(sess))
	}
	return in
}

// -- action menu ------------------------------------------------------------

type actionScreen struct {
	sess *session
	menu *Menu
}

func newActionScreen(sess *session) *actionScreen {
	return &actionScreen{sess: sess, menu: NewMenu([]MenuItem{
		{Value: "index", Label: "index", Desc: "run or resume supervised indexing"},
		{Value: "info", Label: "info", Desc: "versions + status + resume point"},
		{Value: "status", Label: "status", Desc: "current indexing progress, one shot"},
		{Value: "watch", Label: "watch", Desc: "poll status until indexing goes idle"},
		{Value: "health", Label: "health", Desc: "is the active index populated? do counts align?"},
		{Value: "counts", Label: "counts", Desc: "DB vs ES document counts (slow)"},
		{Value: "versions", Label: "versions", Desc: "browse versions: activate, delete, or build into one"},
		{Value: "unlock", Label: "unlock", Desc: "clear a stale index lock (delete-transient)"},
		{Value: "stop", Label: "stop", Desc: "ask a running index to stop"},
	})}
}

func (s *actionScreen) Title() string { return s.sess.target.Label() }
func (s *actionScreen) Init() tea.Cmd { return nil }

func (s *actionScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if key.String() == "q" {
		return s, tea.Quit
	}
	if !s.menu.Update(key) {
		return s, nil
	}
	switch s.menu.Selected().Value {
	case "index":
		return s, push(newIndexablesScreen(s.sess))
	case "versions":
		return s, push(newVersionsIndexableScreen(s.sess))
	case "watch":
		return s, push(newWatchScreen(s.sess))
	case "unlock":
		return s, push(newUnlockScreen(s.sess))
	default:
		return s, push(newOutputScreen(s.sess, s.menu.Selected().Value))
	}
}

func (s *actionScreen) View() string {
	return styleHeading.Render("What do you want to do?") + "\n\n" +
		s.menu.View() +
		styleHelp.Render("↑/↓ move · enter select · esc back · q quit")
}

// -- index wizard: indexables ------------------------------------------------

type indexablesScreen struct {
	sess  *session
	multi *MultiSelect
}

func newIndexablesScreen(sess *session) *indexablesScreen {
	return &indexablesScreen{sess: sess, multi: NewMultiSelect([]MenuItem{
		{Value: "post", Label: "post", Desc: "posts, pages and custom post types"},
		// Only `post` is registered unconditionally; the rest need their
		// ElasticPress feature active or indexing them fails immediately.
		{Value: "term", Label: "term", Desc: "needs the 'terms' feature active"},
		{Value: "user", Label: "user", Desc: "needs the 'users' feature active"},
		{Value: "comment", Label: "comment", Desc: "needs the 'comments' feature active"},
	}, sess.indexables...)}
}

func (s *indexablesScreen) Title() string { return "index" }
func (s *indexablesScreen) Init() tea.Cmd { return nil }

func (s *indexablesScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if !s.multi.Update(key) {
		return s, nil
	}
	s.sess.indexables = s.multi.Selected()
	if contains(s.sess.indexables, "post") {
		return s, push(newPostTypesScreen(s.sess, func(sess *session) Screen {
			return newStrategyScreen(sess)
		}))
	}
	return s, push(newStrategyScreen(s.sess))
}

func (s *indexablesScreen) View() string {
	return styleHeading.Render("Which indexables?") + "\n" +
		styleDim.Render("Each becomes its own supervised phase with its own resume checkpoint.") + "\n\n" +
		s.multi.View() +
		styleHelp.Render("↑/↓ move · space toggle · a all · enter continue · esc back")
}

// newPostTypesScreen takes its successor as a parameter because two wizards
// pass through it: the normal index flow (next: strategy) and the
// build-into-version flow (next: options — the strategy is already fixed).
func newPostTypesScreen(sess *session, next func(*session) Screen) *inputScreen {
	in := newInputScreen("post types", "Restrict to specific post types?",
		"comma-separated, e.g. post,page — empty indexes every post type", sess.postTypes)
	in.submit = func(v string) tea.Cmd {
		sess.postTypes = v
		return push(next(sess))
	}
	return in
}

// -- index wizard: strategy ---------------------------------------------------

type strategyScreen struct {
	sess *session
	menu *Menu
}

func newStrategyScreen(sess *session) *strategyScreen {
	return &strategyScreen{sess: sess, menu: NewMenu([]MenuItem{
		{Value: "new-version", Label: "new version (recommended)",
			Desc: "builds into a fresh index version, activates when verified; search stays up throughout"},
		{Value: "resume", Label: "resume in place",
			Desc: "continue into the version serving search now"},
		{Value: "setup", Label: "rebuild in place",
			Desc: "EMPTIES the live index; search returns nothing until done — hours, on a large site"},
	})}
}

func (s *strategyScreen) Title() string { return "strategy" }
func (s *strategyScreen) Init() tea.Cmd { return nil }

func (s *strategyScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if !s.menu.Update(key) {
		return s, nil
	}
	// A leftover version pin from an earlier "build into version" trip through
	// this session must not leak into a freshly chosen strategy.
	s.sess.intoVersion = 0
	switch s.menu.Selected().Value {
	case "new-version":
		s.sess.strategy = supervise.StrategyNewVersion
	case "setup":
		s.sess.strategy = supervise.StrategySetup
	default:
		s.sess.strategy = supervise.StrategyResume
	}
	return s, push(newOptionsScreen(s.sess))
}

func (s *strategyScreen) View() string {
	return styleHeading.Render("How should the index be built?") + "\n\n" +
		s.menu.View() +
		styleHelp.Render("↑/↓ move · enter select · esc back")
}

// -- index wizard: options form ----------------------------------------------

const (
	optPerPage = iota
	optShowErrors
	optBudget
)

type optionsScreen struct {
	sess    *session
	form    *form
	errText string
}

func newOptionsScreen(sess *session) *optionsScreen {
	return &optionsScreen{sess: sess, form: &form{
		fields: []formField{
			{label: "Objects per cycle (1–5000)", kind: kindText, text: strconv.Itoa(sess.perPage)},
			{label: "Show verbose indexer errors", kind: kindToggle, on: sess.showErrors},
			{label: "Wall-clock budget (e.g. 90m, 6h)", kind: kindText, placeholder: "unlimited"},
		},
		actions: []string{"Continue ▶", "Advanced ▸"},
	}}
}

func (s *optionsScreen) Title() string { return "options" }
func (s *optionsScreen) Init() tea.Cmd { return nil }

func (s *optionsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	action := s.form.Update(key)
	if action == "" {
		s.errText = ""
		return s, nil
	}
	if !s.apply() {
		return s, nil
	}
	if action == "Advanced ▸" {
		return s, push(newAdvancedScreen(s.sess))
	}
	return s, push(newConfirmScreen(s.sess))
}

// apply validates the form into the session; false leaves the cursor on the
// offending field with an explanation.
func (s *optionsScreen) apply() bool {
	perPage, err := strconv.Atoi(strings.TrimSpace(s.form.fields[optPerPage].text))
	if err != nil || perPage < 1 || perPage > 5000 {
		s.errText = "objects per cycle must be a number between 1 and 5000"
		s.form.FocusField(optPerPage)
		return false
	}
	budget, err := parseBudget(s.form.fields[optBudget].text)
	if err != nil {
		s.errText = "budget looks wrong — try 90m, 6h, 1h30m, or leave it empty"
		s.form.FocusField(optBudget)
		return false
	}
	s.sess.perPage = perPage
	s.sess.showErrors = s.form.fields[optShowErrors].on
	s.sess.maxDuration = budget
	return true
}

func (s *optionsScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Indexing options") + "\n\n")
	b.WriteString(s.form.View())
	if s.errText != "" {
		b.WriteString("\n" + styleErr.Render("  ✗ "+s.errText) + "\n")
	}
	b.WriteString(styleHelp.Render("↑/↓ move · type to edit · space toggle · enter next/select · esc back"))
	return b.String()
}

// -- index wizard: advanced options -------------------------------------------

const (
	advResumeFrom = iota
	advStateDir
	advStallTimeout
	advIgnoreLock
)

type advancedScreen struct {
	sess    *session
	form    *form
	errText string
}

func newAdvancedScreen(sess *session) *advancedScreen {
	return &advancedScreen{sess: sess, form: &form{
		fields: []formField{
			{label: "Resume from object ID", kind: kindText,
				text: textIfPositive(sess.resumeFrom), placeholder: "auto (checkpoint / live resume point)"},
			{label: "State directory", kind: kindText,
				text: sess.stateDir, placeholder: "default: ~/.vip-reindex/<target>"},
			{label: "Stall timeout (e.g. 10m)", kind: kindText,
				text: textIfDuration(sess.stallTimeout), placeholder: "default: 10m"},
			{label: "Ignore the state-dir lock", kind: kindToggle, on: sess.ignoreLock},
		},
		actions: []string{"Done ✓"},
	}}
}

func (s *advancedScreen) Title() string { return "advanced" }
func (s *advancedScreen) Init() tea.Cmd { return nil }

func (s *advancedScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if s.form.Update(key) == "" {
		s.errText = ""
		return s, nil
	}
	if !s.apply() {
		return s, nil
	}
	return s, pop()
}

func (s *advancedScreen) apply() bool {
	resumeText := strings.TrimSpace(s.form.fields[advResumeFrom].text)
	var resumeFrom int64
	if resumeText != "" {
		n, err := strconv.ParseInt(resumeText, 10, 64)
		if err != nil || n <= 0 {
			s.errText = "resume ID must be a positive number, or empty for automatic"
			s.form.FocusField(advResumeFrom)
			return false
		}
		if len(s.sess.indexables) > 1 {
			s.errText = "a forced resume ID only makes sense with a single indexable"
			s.form.FocusField(advResumeFrom)
			return false
		}
		if s.sess.strategy == supervise.StrategySetup {
			s.errText = "a rebuild in place starts from the top — a resume ID cannot apply"
			s.form.FocusField(advResumeFrom)
			return false
		}
		resumeFrom = n
	}
	stall, err := parseBudget(s.form.fields[advStallTimeout].text)
	if err != nil {
		s.errText = "stall timeout looks wrong — try 10m or 600"
		s.form.FocusField(advStallTimeout)
		return false
	}
	s.sess.resumeFrom = resumeFrom
	s.sess.stateDir = strings.TrimSpace(s.form.fields[advStateDir].text)
	s.sess.stallTimeout = stall
	s.sess.ignoreLock = s.form.fields[advIgnoreLock].on
	return true
}

func (s *advancedScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Advanced options") + "\n")
	b.WriteString(styleDim.Render("Defaults are right for almost every run; change these only on purpose.") + "\n\n")
	b.WriteString(s.form.View())
	if s.errText != "" {
		b.WriteString("\n" + styleErr.Render("  ✗ "+s.errText) + "\n")
	}
	b.WriteString(styleHelp.Render("↑/↓ move · type to edit · space toggle · enter next/done · esc back"))
	return b.String()
}

func textIfPositive(n int64) string {
	if n > 0 {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

func textIfDuration(d time.Duration) string {
	if d > 0 {
		return d.String()
	}
	return ""
}

// -- confirm ------------------------------------------------------------------

type confirmScreen struct {
	sess      *session
	menu      *Menu
	guard     *inputScreen // typed environment name, when setup targets production
	guardOpen bool
}

func newConfirmScreen(sess *session) *confirmScreen {
	return &confirmScreen{sess: sess, menu: NewMenu([]MenuItem{
		{Value: "run", Label: "Start supervised indexing", Desc: "runs until every phase completes"},
		{Value: "back", Label: "Go back", Desc: "adjust the answers"},
	})}
}

func (s *confirmScreen) Title() string { return "confirm" }
func (s *confirmScreen) Init() tea.Cmd { return nil }

func (s *confirmScreen) needsProductionGuard() bool {
	return s.sess.strategy == supervise.StrategySetup && s.sess.target.LooksLikeProduction()
}

func (s *confirmScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if s.guardOpen {
		return s.updateGuard(msg)
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if !s.menu.Update(key) {
		return s, nil
	}
	switch s.menu.Selected().Value {
	case "back":
		return s, pop()
	case "run":
		if s.needsProductionGuard() {
			s.openGuard()
			return s, textinput.Blink
		}
		return s, push(newRunScreen(s.sess.config()))
	}
	return s, nil
}

func (s *confirmScreen) openGuard() {
	// Every command this tool builds carries --skip-confirm, which suppresses
	// the platform's own "are you sure?" prompt — so the confirmation has to
	// happen here or it happens nowhere.
	in := newInputScreen("confirm", "Type the environment name to confirm", "", "")
	in.submit = nil
	s.guard = in
	s.guardOpen = true
}

func (s *confirmScreen) updateGuard(msg tea.Msg) (Screen, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			s.guardOpen = false
			return s, nil
		case "enter":
			if strings.TrimSpace(s.guard.input.Value()) == s.sess.target.AppEnv {
				return s, push(newRunScreen(s.sess.config()))
			}
			s.guard.errText = "that does not match " + s.sess.target.AppEnv
			return s, nil
		}
	}
	var cmd tea.Cmd
	s.guard.input, cmd = s.guard.input.Update(msg)
	if s.guard.errText != "" {
		s.guard.errText = ""
	}
	return s, cmd
}

// OwnsEsc: while the production guard is open, Esc closes the guard instead
// of popping the whole screen.
func (s *confirmScreen) OwnsEsc() bool { return s.guardOpen }

func (s *confirmScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Ready to run") + "\n\n")

	for _, indexable := range s.sess.indexables {
		b.WriteString(styleDim.Render("  $ ") + styleAccent.Render(s.sess.previewCommand(indexable)) + "\n")
	}
	cfg := s.sess.config()
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  strategy     %s\n", s.sess.strategy))
	b.WriteString(fmt.Sprintf("  state dir    %s\n", cfg.StateDir))
	if cfg.MaxDuration > 0 {
		b.WriteString(fmt.Sprintf("  time budget  %s (stops at a checkpoint; a re-run resumes)\n", cfg.MaxDuration))
	}
	if s.sess.resumeFrom > 0 {
		b.WriteString(styleWarn.Render(fmt.Sprintf("  resume from  id %d (forced — overrides checkpoints)", s.sess.resumeFrom)) + "\n")
	}
	if s.sess.stallTimeout > 0 {
		b.WriteString(fmt.Sprintf("  stall kill   after %s of silence\n", s.sess.stallTimeout))
	}
	if s.sess.ignoreLock {
		b.WriteString(styleWarn.Render("  lock         IGNORED — concurrent runs corrupt checkpoints") + "\n")
	}
	b.WriteString(styleDim.Render("  a deploy or crash mid-run is fine — the supervisor resumes from the last object id\n"))

	if s.sess.strategy == supervise.StrategySetup {
		b.WriteString("\n" + styleErr.Render("  ⚠ rebuild in place EMPTIES the live index until the rebuild finishes") + "\n")
	}
	b.WriteString("\n")

	if s.guardOpen {
		b.WriteString(styleErr.Render("  --setup will DELETE the live index on "+s.sess.target.AppEnv) + "\n\n")
		b.WriteString(s.guard.View())
		return b.String()
	}

	b.WriteString(s.menu.View())
	b.WriteString(styleHelp.Render("↑/↓ move · enter select · esc back"))
	return b.String()
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
