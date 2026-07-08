package ragcore

import "sort"

// День 23: детерминированные (без LLM) этапы второго прохода retrieval.
// Работают над результатом bi-encoder-поиска ([]Hit) и не зависят от провайдера.

// FilterThreshold отсекает кандидатов со score ниже порога (задание: «порог
// отсечения нерелевантных результатов»). Если порог <= 0 — фильтр не применяется.
func FilterThreshold(hits []Hit, min float64) []Hit {
	if min <= 0 {
		return hits
	}
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.Score >= min {
			out = append(out, h)
		}
	}
	return out
}

// MMR (Maximal Marginal Relevance) переупорядочивает кандидатов, балансируя
// релевантность запросу и НЕизбыточность: на каждом шаге берётся кандидат с
// максимумом  lambda*rel - (1-lambda)*max_sim_c_уже_выбранными.
// Убирает near-дубли (в т.ч. несколько похожих чанков одного файла), поднимает
// разнообразие источников. Использует косинус между векторами чанков (векторы
// L2-нормированы ⇒ косинус = скалярное произведение).
//
//	lambda=1   → чистая релевантность (MMR выключен),
//	lambda~0.7 → лёгкая диверсификация (разумный дефолт),
//	lambda=0   → максимум разнообразия.
func MMR(hits []Hit, lambda float64, k int) []Hit {
	if k <= 0 || k > len(hits) {
		k = len(hits)
	}
	if lambda >= 1 || len(hits) <= 1 {
		out := append([]Hit(nil), hits...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
		if k < len(out) {
			out = out[:k]
		}
		return out
	}

	remaining := append([]Hit(nil), hits...)
	selected := make([]Hit, 0, k)
	for len(selected) < k && len(remaining) > 0 {
		bestIdx, bestVal := 0, -1e18
		for i, h := range remaining {
			// максимум сходства с уже выбранными
			var maxSim float64
			for _, s := range selected {
				if sim := dot(h.Chunk.Vector, s.Chunk.Vector); sim > maxSim {
					maxSim = sim
				}
			}
			val := lambda*h.Score - (1-lambda)*maxSim
			if val > bestVal {
				bestVal, bestIdx = val, i
			}
		}
		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}
	return selected
}

// UniqueSources считает число различных файлов-источников среди хитов
// (метрика разнообразия выдачи для отчёта до/после MMR).
func UniqueSources(hits []Hit) int {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Chunk.Source] = true
	}
	return len(seen)
}
