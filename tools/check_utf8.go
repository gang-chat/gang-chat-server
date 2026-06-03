package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var skippedDirectories = map[string]bool{
	".git":         true,
	".cache":       true,
	".idea":        true,
	".vscode":      true,
	"dist":         true,
	"node_modules": true,
	"target":       true,
}

var binaryExtensions = map[string]bool{
	".a":     true,
	".bin":   true,
	".dll":   true,
	".dylib": true,
	".exe":   true,
	".gif":   true,
	".ico":   true,
	".jpg":   true,
	".jpeg":  true,
	".png":   true,
	".so":    true,
	".webp":  true,
	".zip":   true,
}

var mojibakeMarkers = []string{
	"�",
	"Ã",
	"Â",
	"â€",
	"ä¸",
	"ä½",
	"å¥",
	"æœ",
	"鈥",
	"锛",
	"浣犵",
	"娴佺",
	"鍥藉",
	"涓嶇",
	"鐗堟",
}

func main() {
	var failures []string

	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", slash(path), walkErr))
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if skippedDirectories[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if binaryExtensions[strings.ToLower(filepath.Ext(name))] {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", slash(path), err))
			return nil
		}
		if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
			failures = append(failures, slash(path)+": has UTF-8 BOM; save as UTF-8 without BOM")
			return nil
		}
		if !utf8.Valid(data) {
			failures = append(failures, slash(path)+": is not valid UTF-8")
			return nil
		}
		if slash(path) != "tools/check_utf8.go" {
			text := string(data)
			for _, marker := range mojibakeMarkers {
				if strings.Contains(text, marker) {
					failures = append(failures, fmt.Sprintf("%s: contains suspicious mojibake marker %q", slash(path), marker))
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if len(failures) == 0 {
		fmt.Println("UTF-8 check passed.")
		return
	}

	fmt.Fprintln(os.Stderr, "UTF-8 check failed:")
	for _, failure := range failures {
		fmt.Fprintln(os.Stderr, "- "+failure)
	}
	os.Exit(1)
}

func slash(path string) string {
	return filepath.ToSlash(path)
}
