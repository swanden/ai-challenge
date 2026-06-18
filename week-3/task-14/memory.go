package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Layer — тип памяти. Три слоя из задания дня 11.
type Layer int

const (
	ShortTerm Layer = iota // краткосрочная: текущий диалог (сообщения чата)
	Working                // рабочая: TaskContext — данные задачи через сессии
	LongTerm               // долговременная: профиль/решения/знания (.md)
)

func (l Layer) String() string {
	switch l {
	case ShortTerm:
		return "short-term (диалог)"
	case Working:
		return "working (TaskContext)"
	case LongTerm:
		return "long-term (профиль)"
	default:
		return "unknown"
	}
}

// WorkingMem — РАБОЧАЯ память = TaskContext. Это НЕ текущий диалог: она про
// задачу/фичу и живёт через несколько сессий (ресёрч → разработка → тесты).
// Хранит статус: план, что сделано, текущий шаг, решения по задаче.
type WorkingMem struct {
	Task    string            `json:"task"`
	Goal    string            `json:"goal"`
	State   TaskState         `json:"state"`   // этап конечного автомата
	Current string            `json:"current"` // текущий шаг
	Plan    []string          `json:"plan"`    // что предстоит
	Done    []string          `json:"done"`    // что выполнено
	Facts   map[string]string `json:"facts"`   // решения/данные по задаче
}

func (w WorkingMem) empty() bool {
	return w.Task == "" && w.Current == "" && len(w.Plan) == 0 &&
		len(w.Done) == 0 && len(w.Facts) == 0
}

// Memory держит ТРИ слоя РАЗДЕЛЬНО, с разным хранилищем:
//
//	short-term → сообщения чата   (short-term.json)
//	working    → TaskContext      (working.json)
//	long-term  → активный профиль  (из ProfileStore)
type Memory struct {
	dir string

	shortTerm []storedMessage // краткосрочная: диалог
	working   WorkingMem      // рабочая: TaskContext
	longDoc   string          // долговременная: markdown-документ
}

func NewMemory(dir string) *Memory {
	return &Memory{dir: dir, working: WorkingMem{Facts: map[string]string{}}}
}

// --- ЯВНАЯ запись в конкретный слой -------------------------------------

// Dialog → краткосрочная (диалог). Это делает агент сам в Ask.
func (m *Memory) Dialog(role, content string) {
	m.shortTerm = append(m.shortTerm, storedMessage{Role: role, Content: content})
}

// --- рабочая (TaskContext) ---
func (m *Memory) SetTask(name, goal string) {
	m.working.Task, m.working.Goal = name, goal
	if !m.working.State.valid() {
		m.working.State = StatePlanning
	}
}
func (m *Memory) SetPlan(steps ...string) { m.working.Plan = steps }
func (m *Memory) SetCurrent(step string)  { m.working.Current = step }
func (m *Memory) MarkDone(step string) {
	m.working.Done = append(m.working.Done, step)
	out := m.working.Plan[:0]
	for _, s := range m.working.Plan {
		if s != step {
			out = append(out, s)
		}
	}
	m.working.Plan = out
}
func (m *Memory) Decide(key, value string) { m.working.Facts[key] = value }

// State возвращает текущий этап задачи.
func (m *Memory) State() TaskState { return m.working.State }

// Advance — детерминированный переход на следующий этап (с контролем в коде).
func (m *Memory) Advance() (TaskState, error) {
	to, ok := nextState(m.working.State)
	if !ok {
		return m.working.State, fmt.Errorf("задача уже в состоянии %s — двигаться некуда", m.working.State)
	}
	if err := transition(m.working.State, to); err != nil {
		return m.working.State, err
	}
	m.working.State = to
	return to, nil
}

// Goto — явный переход на конкретный этап (тоже под контролем transitions).
func (m *Memory) Goto(to TaskState) error {
	if !to.valid() {
		return fmt.Errorf("неизвестное состояние %q", to)
	}
	if err := transition(m.working.State, to); err != nil {
		return err
	}
	m.working.State = to
	return nil
}

// ResetTask очищает рабочую память (задача завершена), не трогая остальное.
func (m *Memory) ResetTask() {
	m.working = WorkingMem{Facts: map[string]string{}, State: StatePlanning}
}

// --- долговременная: содержимое приходит из активного профиля ---
func (m *Memory) SetLongDoc(md string) { m.longDoc = md }

// --- Состав запроса: ЯВНО выбираем, какие слои инжектить ----------------

// Policy — какие слои попадут в запрос. Под задачу слои можно выключать.
type Policy struct {
	Profile    bool // долговременная (активный профиль)
	Working    bool // рабочая (TaskContext)
	ShortTermN int  // сколько последних реплик диалога взять (0 — ни одной)
}

func FullPolicy() Policy { return Policy{Profile: true, Working: true, ShortTermN: 20} }

// Build собирает system-блоки и сообщения из ВЫБРАННЫХ слоёв.
// long-term и working → System; короткий диалог → messages.
func (m *Memory) Build(query string, p Policy) (system []anthropic.TextBlockParam, msgs []anthropic.MessageParam, used []Layer) {
	if p.Profile && strings.TrimSpace(m.longDoc) != "" {
		system = append(system, anthropic.TextBlockParam{Text: "[ДОЛГОВРЕМЕННАЯ ПАМЯТЬ — профиль]\n" + m.longDoc})
		used = append(used, LongTerm)
	}
	if p.Working && !m.working.empty() {
		system = append(system, anthropic.TextBlockParam{Text: "[РАБОЧАЯ ПАМЯТЬ — TaskContext]\n" + m.renderWorking()})
		if m.working.State.valid() {
			system = append(system, anthropic.TextBlockParam{Text: fmt.Sprintf(
				"[ЭТАП ЗАДАЧИ]\nТекущий state: %s.\nНа этом этапе: %s.\nРаботай строго в рамках этого этапа, не перепрыгивай вперёд.",
				m.working.State, expectedAction[m.working.State])})
		}
		used = append(used, Working)
	}

	hist := m.shortTerm
	if p.ShortTermN <= 0 {
		hist = nil
	} else if len(hist) > p.ShortTermN {
		hist = hist[len(hist)-p.ShortTermN:]
	}
	if len(hist) > 0 {
		used = append(used, ShortTerm)
	}
	for _, msg := range hist {
		block := anthropic.NewTextBlock(msg.Content)
		if msg.Role == roleAssistant {
			msgs = append(msgs, anthropic.NewAssistantMessage(block))
			continue
		}
		msgs = append(msgs, anthropic.NewUserMessage(block))
	}
	msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(query)))
	return system, msgs, used
}

func (m *Memory) renderWorking() string {
	var b strings.Builder
	if m.working.Task != "" {
		fmt.Fprintf(&b, "Задача: %s\n", m.working.Task)
	}
	if m.working.Goal != "" {
		fmt.Fprintf(&b, "Цель: %s\n", m.working.Goal)
	}
	if m.working.State != "" {
		fmt.Fprintf(&b, "Этап (state): %s — %s\n", m.working.State, expectedAction[m.working.State])
	}
	if m.working.Current != "" {
		fmt.Fprintf(&b, "Текущий шаг: %s\n", m.working.Current)
	}
	if len(m.working.Done) > 0 {
		fmt.Fprintf(&b, "Сделано: %s\n", strings.Join(m.working.Done, "; "))
	}
	if len(m.working.Plan) > 0 {
		fmt.Fprintf(&b, "Осталось: %s\n", strings.Join(m.working.Plan, "; "))
	}
	if len(m.working.Facts) > 0 {
		b.WriteString("Решения:\n")
		b.WriteString(renderKV(m.working.Facts))
	}
	return b.String()
}

func renderKV(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", k, m[k])
	}
	return b.String()
}

// --- Инспекция: что лежит в каждом слое ---------------------------------

func (m *Memory) Inspect() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d сообщений\n", ShortTerm, len(m.shortTerm))
	for _, msg := range m.shortTerm {
		fmt.Fprintf(&b, "    %s: %s\n", msg.Role, oneLine(msg.Content, 70))
	}
	fmt.Fprintf(&b, "%s — задача %q\n", Working, m.working.Task)
	if s := strings.TrimRight(m.renderWorking(), "\n"); s != "" {
		for _, line := range strings.Split(s, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	fmt.Fprintf(&b, "%s — %d символов (активный профиль)\n", LongTerm, len(m.longDoc))
	if s := strings.TrimRight(m.longDoc, "\n"); s != "" {
		for _, line := range strings.Split(s, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return b.String()
}

// --- РАЗДЕЛЬНОЕ хранение: каждый слой в своём файле/формате --------------

// Save сохраняет слои, которыми управляет агент: диалог и TaskContext.
// Долговременный слой здесь = активный профиль, его персистит ProfileStore.
func (m *Memory) Save() error {
	if m.dir == "" {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(m.dir, "short-term.json"), m.shortTerm); err != nil {
		return err
	}
	return writeJSON(filepath.Join(m.dir, "working.json"), m.working)
}

// HasLongDoc — есть ли непустой долговременный документ.
func (m *Memory) HasLongDoc() bool { return strings.TrimSpace(m.longDoc) != "" }

func LoadMemory(dir string) (*Memory, error) {
	m := NewMemory(dir)
	_ = readJSON(filepath.Join(dir, "short-term.json"), &m.shortTerm)
	_ = readJSON(filepath.Join(dir, "working.json"), &m.working)
	if m.working.Facts == nil {
		m.working.Facts = map[string]string{}
	}
	return m, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(data))
}

func writeFile(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// --- аксессоры для интерактивных команд ---
func (m *Memory) taskName() string { return m.working.Task }
func (m *Memory) taskGoal() string { return m.working.Goal }

// LongDoc — текущее содержимое долговременного слоя (активный профиль).
func (m *Memory) LongDoc() string { return m.longDoc }

// TaskName — имя текущей задачи.
func (m *Memory) TaskName() string { return m.working.Task }

// TaskGoal — цель текущей задачи.
func (m *Memory) TaskGoal() string { return m.working.Goal }
