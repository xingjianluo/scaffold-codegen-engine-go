package engine

import (
	"path/filepath"
)

func join(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(filepath.ToSlash(rel)))
}

func filepathClean(p string) string {
	return filepath.ToSlash(filepath.Clean(filepath.ToSlash(p)))
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == filepath.Separator {
			return p[i+1:]
		}
	}
	return p
}
