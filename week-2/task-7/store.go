package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// storedMessage — DTO для персистентности. Намеренно не тащим сюда
// типы SDK: на диске лежит простой человекочитаемый формат
// {"role": "...", "content": "..."}, независимый от версии библиотеки.
type storedMessage struct {
	Role    string `json:"role"`    // "user" | "assistant"
	Content string `json:"content"` // текст реплики
}

// Store — абстракция хранилища истории диалога.
// Сегодня реализация JSON, завтра можно подменить на SQLite —
// агент об этом знать не должен.
type Store interface {
	Load() ([]storedMessage, error)
	Save(history []storedMessage) error
}

// JSONStore хранит историю в одном JSON-файле.
type JSONStore struct {
	path string
}

func NewJSONStore(path string) *JSONStore {
	return &JSONStore{path: path}
}

// Load читает историю из файла. Если файла нет — это не ошибка,
// просто пустая история (первый запуск).
func (s *JSONStore) Load() ([]storedMessage, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read %q: %w", s.path, err)
	}

	var history []storedMessage
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("store: parse %q: %w", s.path, err)
	}
	return history, nil
}

// Save атомарно перезаписывает файл: пишем во временный и переименовываем,
// чтобы при падении посреди записи не остаться с битым JSON.
func (s *JSONStore) Save(history []storedMessage) error {
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".history-*.tmp")
	if err != nil {
		return fmt.Errorf("store: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // на случай ошибки до rename

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
