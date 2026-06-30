package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Index — локальный индекс: метаданные сборки + чанки с векторами. Хранится в JSON.
// Спроектирован под будущую обёртку MCP-тулом семантического поиска: метод Search
// принимает Embedder и запрос, отдаёт top-k — ровно то, что дёрнет MCP-сервер.
type Index struct {
	Model     string  `json:"model"`
	Dim       int     `json:"dim"`
	Strategy  string  `json:"strategy"`
	ChunkSize int     `json:"chunk_size,omitempty"`
	Overlap   int     `json:"overlap,omitempty"`
	Chunks    []Chunk `json:"chunks"`
}

// embedAll прогоняет чанки через эмбеддер и заполняет векторы (+ Dim/Model).
func (idx *Index) embedAll(ctx context.Context, e Embedder, chunks []Chunk) error {
	idx.Model = e.Name()
	idx.Chunks = make([]Chunk, len(chunks))
	for i, c := range chunks {
		vec, err := e.Embed(ctx, c.Text)
		if err != nil {
			return fmt.Errorf("чанк %s: %w", c.ID, err)
		}
		if idx.Dim == 0 {
			idx.Dim = len(vec)
		}
		c.Vector = vec
		idx.Chunks[i] = c
		fmt.Printf("\r  эмбеддинг %d/%d", i+1, len(chunks))
	}
	fmt.Println()
	return nil
}

// Save пишет индекс в JSON атомарно (tmp + rename).
func (idx *Index) Save(path string) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadIndex читает индекс из JSON (понадобится будущему MCP-тулу поиска).
func LoadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// Hit — результат поиска: чанк + косинусная близость к запросу.
type Hit struct {
	Chunk Chunk
	Score float64
}

// Search — семантический поиск top-k по косинусной близости. Векторы L2-нормированы,
// поэтому косинус = скалярное произведение. Это «зерно» будущего MCP-тула поиска
// (bi-encoder / первый этап; реранкинг cross-encoder'ом — отдельный будущий день).
func (idx *Index) Search(ctx context.Context, e Embedder, query string, k int) ([]Hit, error) {
	q, err := e.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(idx.Chunks))
	for _, c := range idx.Chunks {
		hits = append(hits, Hit{Chunk: c, Score: dot(q, c.Vector)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

func dot(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float64
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
