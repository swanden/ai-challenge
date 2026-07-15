package task_32

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile — профиль пользователя: предпочтения (стиль, формат, ограничения).
// Хранится как редактируемый markdown (как long-term-слой из дня 11), но теперь
// профилей МНОГО и между ними можно переключаться.
type Profile struct {
	ID  string
	Doc string // markdown с предпочтениями
}

func (p Profile) empty() bool { return strings.TrimSpace(p.Doc) == "" }

// ProfileStore хранит профили РАЗДЕЛЬНО: каждый — свой файл <id>.md в каталоге.
// Плюс есть встроенные демо-профили (none / junior / senior-go).
type ProfileStore struct {
	dir     string
	builtin map[string]string
}

func NewProfileStore(dir string) *ProfileStore {
	return &ProfileStore{dir: dir, builtin: builtinProfiles()}
}

// Load берёт профиль по id: сначала файл <id>.md, иначе встроенный.
func (s *ProfileStore) Load(id string) (Profile, error) {
	if s.dir != "" {
		if data, err := os.ReadFile(filepath.Join(s.dir, id+".md")); err == nil {
			return Profile{ID: id, Doc: string(data)}, nil
		}
	}
	if doc, ok := s.builtin[id]; ok {
		return Profile{ID: id, Doc: doc}, nil
	}
	return Profile{ID: id}, os.ErrNotExist
}

// List — доступные id (встроенные + файлы в каталоге), без дублей.
func (s *ProfileStore) List() []string {
	seen := map[string]bool{}
	var ids []string
	for id := range s.builtin {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if s.dir != "" {
		entries, _ := os.ReadDir(s.dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				id := strings.TrimSuffix(e.Name(), ".md")
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// Save пишет профиль в <id>.md (пользователь может потом править руками).
func (s *ProfileStore) Save(p Profile) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, p.ID+".md"), []byte(p.Doc), 0o644)
}

// builtinProfiles — демо-профили для -report и первого запуска.
func builtinProfiles() map[string]string {
	return map[string]string{
		"none": "",
		"junior": `# Профиль: Junior-разработчик
- Уровень: начинающий
- Стиль: дружелюбно, простыми словами, без жаргона
- Формат: пошагово, с маленьким примером кода
- Ограничения: объясняй базовые термины, не предполагай глубоких знаний
`,
		"senior-go": `# Профиль: Senior Go-разработчик
- Уровень: senior (распределённые системы)
- Стиль: кратко и по делу, без воды
- Формат: сразу суть + идиоматичный Go-пример, отметить trade-offs
- Ограничения: язык Go; минимум зависимостей; не объяснять азы
`,
	}
}
