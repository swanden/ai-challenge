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
		return "long-term (.md)"
	default:
		return "unknown"
	}
}

// Profile — удобная структура, из которой собирается долговременный .md-документ.
type Profile struct {
	Role        string
	Style       string
	Language    string
	Constraints []string
}

func profileToMarkdown(p Profile) string {
	var b strings.Builder
	b.WriteString("# Профиль\n")
	if p.Role != "" {
		fmt.Fprintf(&b, "- Роль: %s\n", p.Role)
	}
	if p.Style != "" {
		fmt.Fprintf(&b, "- Стиль ответа: %s\n", p.Style)
	}
	if p.Language != "" {
		fmt.Fprintf(&b, "- Язык: %s\n", p.Language)
	}
	if len(p.Constraints) > 0 {
		fmt.Fprintf(&b, "- Ограничения: %s\n", strings.Join(p.Constraints, "; "))
	}
	return b.String()
}

// WorkingMem — РАБОЧАЯ память = TaskContext. Это НЕ текущий диалог: она про
// задачу/фичу и живёт через несколько сессий (ресёрч → разработка → тесты).
// Хранит статус: план, что сделано, текущий шаг, решения по задаче.
type WorkingMem struct {
	Task    string            `json:"task"`
	Goal    string            `json:"goal"`
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
//	long-term  → редактируемый .md (long-term.md)
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
func (m *Memory) SetTask(name, goal string) { m.working.Task, m.working.Goal = name, goal }
func (m *Memory) SetPlan(steps ...string)   { m.working.Plan = steps }
func (m *Memory) SetCurrent(step string)    { m.working.Current = step }
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

// ResetTask очищает рабочую память (задача завершена), не трогая остальное.
func (m *Memory) ResetTask() { m.working = WorkingMem{Facts: map[string]string{}} }

// --- долговременная (.md) ---
func (m *Memory) SetProfile(p Profile) { m.longDoc = profileToMarkdown(p) }
func (m *Memory) SetLongDoc(md string) { m.longDoc = md }
func (m *Memory) AppendKnowledge(line string) {
	if m.longDoc != "" && !strings.HasSuffix(m.longDoc, "\n") {
		m.longDoc += "\n"
	}
	m.longDoc += "- " + line + "\n"
}

// --- Состав запроса: ЯВНО выбираем, какие слои инжектить ----------------

// Policy — какие слои попадут в запрос. Под задачу слои можно выключать.
type Policy struct {
	Profile    bool // долговременная (.md)
	Working    bool // рабочая (TaskContext)
	ShortTermN int  // сколько последних реплик диалога взять (0 — ни одной)
}

func FullPolicy() Policy { return Policy{Profile: true, Working: true, ShortTermN: 20} }

// Build собирает system-блоки и сообщения из ВЫБРАННЫХ слоёв.
// long-term и working → System; короткий диалог → messages.
func (m *Memory) Build(query string, p Policy) (system []anthropic.TextBlockParam, msgs []anthropic.MessageParam, used []Layer) {
	if p.Profile && strings.TrimSpace(m.longDoc) != "" {
		system = append(system, anthropic.TextBlockParam{Text: "[ДОЛГОВРЕМЕННАЯ ПАМЯТЬ — profile.md]\n" + m.longDoc})
		used = append(used, LongTerm)
	}
	if p.Working && !m.working.empty() {
		system = append(system, anthropic.TextBlockParam{Text: "[РАБОЧАЯ ПАМЯТЬ — TaskContext]\n" + m.renderWorking()})
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
	fmt.Fprintf(&b, "%s — %d символов markdown\n", LongTerm, len(m.longDoc))
	if s := strings.TrimRight(m.longDoc, "\n"); s != "" {
		for _, line := range strings.Split(s, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return b.String()
}

// --- РАЗДЕЛЬНОЕ хранение: каждый слой в своём файле/формате --------------

// Save сохраняет слои, которыми управляет АГЕНТ: диалог и TaskContext.
// long-term.md сюда НЕ входит — это твой авторский файл (см. SaveLong).
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

// SaveLong пишет long-term.md. Вызывается ЯВНО (например, по команде /know),
// чтобы случайно не затереть файл, который ты ведёшь руками.
func (m *Memory) SaveLong() error {
	if m.dir == "" {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	return writeFile(filepath.Join(m.dir, "long-term.md"), m.longDoc)
}

// SaveAll — все три слоя (удобно для демонстрации/сидирования).
func (m *Memory) SaveAll() error {
	if err := m.Save(); err != nil {
		return err
	}
	return m.SaveLong()
}

// HasLongDoc — есть ли непустой долговременный документ.
func (m *Memory) HasLongDoc() bool { return strings.TrimSpace(m.longDoc) != "" }

func LoadMemory(dir string) (*Memory, error) {
	m := NewMemory(dir)
	_ = readJSON(filepath.Join(dir, "short-term.json"), &m.shortTerm)
	_ = readJSON(filepath.Join(dir, "working.json"), &m.working)
	if data, err := os.ReadFile(filepath.Join(dir, "long-term.md")); err == nil {
		m.longDoc = string(data)
	}
	if m.working.Facts == nil {
		m.working.Facts = map[string]string{}
	}
	return m, nil
}

// ReloadLongDoc перечитывает long-term.md (если правили файл руками).
func (m *Memory) ReloadLongDoc() error {
	data, err := os.ReadFile(filepath.Join(m.dir, "long-term.md"))
	if err != nil {
		return err
	}
	m.longDoc = string(data)
	return nil
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
