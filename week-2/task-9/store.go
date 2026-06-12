package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// storedMessage — одна реплика на диске.
type storedMessage struct {
	Role    string `json:"role"`    // "user" | "assistant"
	Content string `json:"content"` // текст реплики
}

// State — всё, что персистим: отдельно конспект и отдельно последние сообщения.
// Именно "храните summary отдельно" из задания дня 9.
type State struct {
	Summary  string          `json:"summary"`
	Messages []storedMessage `json:"messages"`
}

// Store — абстракция хранилища состояния диалога.
type Store interface {
	Load() (State, error)
	Save(state State) error
}

// JSONStore хранит состояние в одном JSON-файле.
type JSONStore struct {
	path string
}

func NewJSONStore(path string) *JSONStore { return &JSONStore{path: path} }

func (s *JSONStore) Load() (State, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("store: read %q: %w", s.path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("store: parse %q: %w", s.path, err)
	}
	return st, nil
}

func (s *JSONStore) Save(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".history-*.tmp")
	if err != nil {
		return fmt.Errorf("store: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: rename: %w", err)
	}
	return nil
}

// MemoryStore — реализация без диска (для отчёта: чистый агент на каждый прогон).
type MemoryStore struct {
	state State
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Load() (State, error) { return m.state, nil }

func (m *MemoryStore) Save(state State) error {
	cp := state
	cp.Messages = append([]storedMessage(nil), state.Messages...)
	m.state = cp
	return nil
}
