package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// День 18. Персистентное хранилище наблюдений (JSON) + агрегация.
// Ключ — координаты (а не сырой ввод), чтобы "Berlin" и "Берлин" не плодили дубли.

type Sample struct {
	Time time.Time `json:"time"`
	Temp float64   `json:"temp"`
	Wind float64   `json:"wind"`
	Code int       `json:"code"`
}

type Tracked struct {
	Location    string   `json:"location"` // как ввёл пользователь (для показа)
	Name        string   `json:"name"`     // нормализованное имя из геокодера
	Country     string   `json:"country"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	IntervalSec int      `json:"interval_sec"`
	Samples     []Sample `json:"samples"`
}

// key — канонический ключ города (координаты). Дедуп по нему.
func (t *Tracked) key() string { return fmt.Sprintf("%.4f,%.4f", t.Lat, t.Lon) }

type Store struct {
	mu      sync.Mutex
	path    string
	Tracked map[string]*Tracked `json:"tracked"`
}

func loadStore(path string) *Store {
	s := &Store{path: path, Tracked: map[string]*Tracked{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var disk struct {
		Tracked map[string]*Tracked `json:"tracked"`
	}
	if err := json.Unmarshal(data, &disk); err == nil && disk.Tracked != nil {
		s.Tracked = disk.Tracked
	}
	return s
}

func (s *Store) saveLocked() {
	data, err := json.MarshalIndent(struct {
		Tracked map[string]*Tracked `json:"tracked"`
	}{s.Tracked}, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Upsert регистрирует/обновляет город по координатному ключу (сохраняя историю).
func (s *Store) Upsert(t *Tracked) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.Tracked[t.key()]; ok {
		t.Samples = old.Samples
	}
	s.Tracked[t.key()] = t
	s.saveLocked()
}

// Append добавляет наблюдение по координатному ключу.
func (s *Store) Append(key string, smp Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Tracked[key]
	if !ok {
		return
	}
	t.Samples = append(t.Samples, smp)
	s.saveLocked()
}

// Has сообщает, отслеживается ли ключ (для остановки осиротевших сборщиков).
func (s *Store) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Tracked[key]
	return ok
}

// Remove удаляет город по вводу/имени (без учёта регистра). Возвращает ключ и имя.
func (s *Store) Remove(query string) (key, name string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	for k, t := range s.Tracked {
		if strings.ToLower(t.Location) == q || strings.ToLower(t.Name) == q {
			delete(s.Tracked, k)
			s.saveLocked()
			return k, t.Name, true
		}
	}
	return "", "", false
}

// RemoveAll очищает всё отслеживание. Возвращает число удалённых.
func (s *Store) RemoveAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.Tracked)
	s.Tracked = map[string]*Tracked{}
	s.saveLocked()
	return n
}

func (s *Store) List() []*Tracked {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tracked, 0, len(s.Tracked))
	for _, t := range s.Tracked {
		out = append(out, t)
	}
	return out
}

// Summary — агрегат. query пуст = по всем; иначе по совпадению ввода/имени.
func (s *Store) Summary(query string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	pick := func(t *Tracked) string {
		n := len(t.Samples)
		if n == 0 {
			return fmt.Sprintf("%s, %s: наблюдений пока нет (интервал %d с)", t.Name, t.Country, t.IntervalSec)
		}
		min, max, sum := t.Samples[0].Temp, t.Samples[0].Temp, 0.0
		for _, smp := range t.Samples {
			if smp.Temp < min {
				min = smp.Temp
			}
			if smp.Temp > max {
				max = smp.Temp
			}
			sum += smp.Temp
		}
		first, last := t.Samples[0], t.Samples[n-1]
		span := last.Time.Sub(first.Time).Round(time.Second)
		return fmt.Sprintf(
			"%s, %s: наблюдений %d за %s · t сейчас %.1f°C (min %.1f / avg %.1f / max %.1f) · обновление каждые %d с",
			t.Name, t.Country, n, span, last.Temp, min, sum/float64(n), max, t.IntervalSec,
		)
	}

	if q := strings.ToLower(strings.TrimSpace(query)); q != "" {
		for _, t := range s.Tracked {
			if strings.ToLower(t.Location) == q || strings.ToLower(t.Name) == q {
				return pick(t)
			}
		}
		return "город не отслеживается: " + query + " (добавь через track_location)"
	}

	if len(s.Tracked) == 0 {
		return "пока ничего не отслеживается — добавь город через track_location"
	}
	out := "Сводка по отслеживаемым городам:\n"
	for _, t := range s.Tracked {
		out += "  • " + pick(t) + "\n"
	}
	return out
}
