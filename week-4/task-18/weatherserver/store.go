package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// День 18. Персистентное хранилище наблюдений (JSON) + агрегация.
// Сервер по расписанию собирает погоду по отслеживаемым городам и копит сюда;
// тул weather_summary отдаёт агрегированный результат.

type Sample struct {
	Time time.Time `json:"time"`
	Temp float64   `json:"temp"`
	Wind float64   `json:"wind"`
	Code int       `json:"code"`
}

type Tracked struct {
	Location    string   `json:"location"` // как ввёл пользователь
	Name        string   `json:"name"`     // нормализованное имя из геокодера
	Country     string   `json:"country"`
	Lat         float64  `json:"lat"`
	Lon         float64  `json:"lon"`
	IntervalSec int      `json:"interval_sec"`
	Samples     []Sample `json:"samples"`
}

// Store — потокобезопасное JSON-хранилище. Сохраняется атомарно (tmp+rename).
type Store struct {
	mu      sync.Mutex
	path    string
	Tracked map[string]*Tracked `json:"tracked"`
}

func loadStore(path string) *Store {
	s := &Store{path: path, Tracked: map[string]*Tracked{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s // нет файла — пустое хранилище
	}
	var disk struct {
		Tracked map[string]*Tracked `json:"tracked"`
	}
	if err := json.Unmarshal(data, &disk); err == nil && disk.Tracked != nil {
		s.Tracked = disk.Tracked
	}
	return s
}

// saveLocked пишет на диск; вызывающий держит s.mu.
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

// Upsert регистрирует/обновляет отслеживаемый город (сохраняя накопленные samples).
func (s *Store) Upsert(t *Tracked) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.Tracked[t.Location]; ok {
		t.Samples = old.Samples // не теряем историю при смене интервала
	}
	s.Tracked[t.Location] = t
	s.saveLocked()
}

// Append добавляет одно наблюдение к городу и сохраняет.
func (s *Store) Append(location string, smp Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Tracked[location]
	if !ok {
		return
	}
	t.Samples = append(t.Samples, smp)
	s.saveLocked()
}

// List возвращает снимок отслеживаемых городов (без samples — для краткого вывода).
func (s *Store) List() []*Tracked {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Tracked, 0, len(s.Tracked))
	for _, t := range s.Tracked {
		out = append(out, t)
	}
	return out
}

// Summary — агрегированный результат. Если location пуст — по всем городам.
func (s *Store) Summary(location string) string {
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

	if location != "" {
		t, ok := s.Tracked[location]
		if !ok {
			return "город не отслеживается: " + location + " (добавь через track_location)"
		}
		return pick(t)
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
