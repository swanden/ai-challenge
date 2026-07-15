package task_32

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Invariant — ограничение, которое агент не имеет права нарушать.
// Kind: "system" (подкапотное) или "user" (заданное пользователем).
type Invariant struct {
	ID   string   `json:"id"`
	Text string   `json:"text"` // человекочитаемое правило
	Kind string   `json:"kind"` // system | user
	Deny []string `json:"deny"` // стоп-слова для детерминированной проверки (lower-case)
}

// InvariantSet хранит инварианты ОТДЕЛЬНО от диалога (свой файл invariants.json).
type InvariantSet struct {
	dir  string
	list []Invariant
}

func NewInvariantSet(dir string) *InvariantSet {
	s := &InvariantSet{dir: dir}
	s.list = append(s.list, systemInvariants()...)
	s.load() // пользовательские поверх системных
	return s
}

// systemInvariants — подкапотные ограничения (юзер может о них не знать).
func systemInvariants() []Invariant {
	return []Invariant{
		{ID: "stack-go", Kind: "system",
			Text: "Стек только Go. Не предлагать другие языки (Java, Python, Rust…) без явного снятия инварианта.",
			Deny: []string{"на java", "на python", "на rust", "напиши на c#", "перепиши на java"}},
		{ID: "no-secrets", Kind: "system",
			Text: "Не хардкодить секреты/ключи в коде; только через переменные окружения.",
			Deny: []string{"захардкоди ключ", "впиши пароль в код", "secret в коде"}},
	}
}

func (s *InvariantSet) All() []Invariant { return s.list }

func (s *InvariantSet) Add(text string, deny []string) Invariant {
	inv := Invariant{
		ID:   fmt.Sprintf("user-%d", len(s.list)+1),
		Text: text, Kind: "user", Deny: lowerAll(deny),
	}
	s.list = append(s.list, inv)
	_ = s.save()
	return inv
}

// Render — блок для System prompt: агент обязан учитывать эти правила.
func (s *InvariantSet) Render() string {
	if len(s.list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Ты ОБЯЗАН соблюдать инварианты ниже. Если запрос им противоречит — ")
	b.WriteString("откажись и объясни, какой инвариант нарушается; предложи допустимую альтернативу.\n")
	for _, inv := range s.list {
		fmt.Fprintf(&b, "- [%s] %s\n", inv.ID, inv.Text)
	}
	return b.String()
}

// CheckCode — детерминированная проверка (способ 1): ищет стоп-слова в тексте.
// Возвращает нарушенные инварианты.
func (s *InvariantSet) CheckCode(text string) []Invariant {
	low := strings.ToLower(text)
	var hit []Invariant
	for _, inv := range s.list {
		for _, d := range inv.Deny {
			if d != "" && strings.Contains(low, d) {
				hit = append(hit, inv)
				break
			}
		}
	}
	return hit
}

func (s *InvariantSet) load() {
	if s.dir == "" {
		return
	}
	var user []Invariant
	if err := readJSON(filepath.Join(s.dir, "invariants.json"), &user); err == nil {
		s.list = append(s.list, user...)
	}
}

func (s *InvariantSet) save() error {
	if s.dir == "" {
		return nil
	}
	var user []Invariant
	for _, inv := range s.list {
		if inv.Kind == "user" {
			user = append(user, inv)
		}
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.dir, "invariants.json"), user)
}

func lowerAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}
