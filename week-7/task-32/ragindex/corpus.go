package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Doc — один документ корпуса, прочитанный в память.
type Doc struct {
	Path string // полный путь
	Rel  string // путь относительно корня корпуса (короткий source в метаданных)
	Name string // имя файла (title/file в метаданных)
	Ext  string // расширение в нижнем регистре (.go/.md/.txt) — выбор стратегии structural
	Text string
}

// loadCorpus рекурсивно собирает файлы с подходящими расширениями из root.
// Каталоги из skipDirs и скрытые (.git и т.п.) пропускаются.
func loadCorpus(root string, exts, skipDirs []string, skipFile map[string]bool) ([]Doc, error) {
	extSet := toSet(exts)
	skipSet := toSet(skipDirs)
	var docs []Doc
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path == root { // сам корень не фильтруем (напр. -docs .)
				return nil
			}
			if skipSet[strings.ToLower(d.Name())] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if isArtifact(d.Name()) { // не индексируем собственный вывод
			return nil
		}
		if skipFile[strings.ToLower(d.Name())] { // день 32: devlog и прочее не-документация
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !extSet[ext] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		docs = append(docs, Doc{Path: path, Rel: rel, Name: d.Name(), Ext: ext, Text: string(b)})
		return nil
	})
	sort.Slice(docs, func(i, j int) bool { return docs[i].Rel < docs[j].Rel })
	return docs, err
}

// isArtifact — собственные выходные файлы пайплайна; их не индексируем.
func isArtifact(name string) bool {
	switch name {
	case "comparison.md", "index-fixed.json", "index-structural.json":
		return true
	}
	return false
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		if t := strings.ToLower(strings.TrimSpace(s)); t != "" {
			m[t] = true
		}
	}
	return m
}
