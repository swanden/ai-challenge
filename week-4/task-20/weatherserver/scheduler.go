package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Scheduler управляет фоновыми сборщиками: по одному на город (ключ — координаты).
// При повторном Start для того же ключа старый сборщик останавливается (без дублей).
type Scheduler struct {
	mu      sync.Mutex
	base    context.Context
	store   *Store
	cancels map[string]context.CancelFunc
}

func NewScheduler(base context.Context, store *Store) *Scheduler {
	return &Scheduler{base: base, store: store, cancels: map[string]context.CancelFunc{}}
}

// Start (пере)запускает сбор по городу: сразу + далее каждые IntervalSec секунд.
func (s *Scheduler) Start(t *Tracked) {
	key := t.key()
	s.mu.Lock()
	if cancel, ok := s.cancels[key]; ok {
		cancel() // останавливаем прежний сборщик этого города
	}
	ctx, cancel := context.WithCancel(s.base)
	s.cancels[key] = cancel
	s.mu.Unlock()

	go s.run(ctx, key, t)
}

// Stop останавливает сборщик по ключу.
func (s *Scheduler) Stop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.cancels[key]; ok {
		cancel()
		delete(s.cancels, key)
	}
}

func (s *Scheduler) run(ctx context.Context, key string, t *Tracked) {
	collect(ctx, s.store, key, t)
	iv := t.IntervalSec
	if iv <= 0 {
		iv = 60
	}
	ticker := time.NewTicker(time.Duration(iv) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.store.Has(key) { // город сняли с отслеживания — выходим
				return
			}
			collect(ctx, s.store, key, t)
		}
	}
}

func collect(ctx context.Context, store *Store, key string, t *Tracked) {
	w, err := currentWeather(ctx, t.Lat, t.Lon, "celsius")
	if err != nil {
		log.Printf("[collector] %s: ошибка сбора: %v", t.Name, err)
		return
	}
	store.Append(key, Sample{Time: time.Now(), Temp: w.Temp, Wind: w.Wind, Code: w.Code})
	log.Printf("[collector] %s: %.1f°C записано", t.Name, w.Temp)
}
