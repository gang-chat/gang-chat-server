package storage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReleasePrefix is where release sync keeps the client's installers and update
// feeds. GET /assets/releases/<name> serves them, next to the chat's assets.
const ReleasePrefix = "assets/releases/"

// tempPrefix marks a file Put is still writing. Keys can't have a segment that
// starts with a dot, so these never show up as files.
const tempPrefix = ".put-"

var ErrInvalidKey = errors.New("invalid storage key")

// Files is the server's file storage: one directory holding the chat's assets
// (assets/<asset id>/<filename>) and the client releases (ReleasePrefix). Keys
// are slash-separated paths below it.
type Files struct {
	root string
}

// File is one stored file.
type File struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// NewFiles opens the storage directory at root, creating it if needed.
func NewFiles(root string) (*Files, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("storage_path is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}
	return &Files{root: abs}, nil
}

// Root is the storage directory.
func (f *Files) Root() string { return f.root }

// path maps key to its file, refusing keys that would leave the root or name a
// hidden file.
func (f *Files) path(key string) (string, error) {
	if key == "" || len(key) > 2048 || strings.ContainsAny(key, "\\\x00") {
		return "", ErrInvalidKey
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return "", ErrInvalidKey
		}
	}
	return filepath.Join(f.root, filepath.FromSlash(key)), nil
}

// Put stores body at key. It writes a temporary file next to it and renames it
// into place, so a reader gets the old file or the new one, never half of it.
func (f *Files) Put(key string, body io.Reader) error {
	target, err := f.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// Open opens the file at key. A missing key, or one that is a folder, is
// fs.ErrNotExist.
func (f *Files) Open(key string) (*os.File, fs.FileInfo, error) {
	target, err := f.path(key)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, nil, &fs.PathError{Op: "open", Path: target, Err: fs.ErrNotExist}
	}
	return file, info, nil
}

// Stat describes the file at key.
func (f *Files) Stat(key string) (File, error) {
	file, info, err := f.Open(key)
	if err != nil {
		return File{}, err
	}
	file.Close()
	return File{Key: key, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// Delete removes the file at key, and the folders it leaves empty. A missing
// key is not an error.
func (f *Files) Delete(key string) error {
	target, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(target); dir != f.root && strings.HasPrefix(dir, f.root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

// List returns the files whose keys start with prefix, sorted by key.
func (f *Files) List(prefix string) ([]File, error) {
	start := f.root
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dir, err := f.path(prefix[:i])
		if err != nil {
			return nil, err
		}
		start = dir
	}
	var files []File
	err := filepath.WalkDir(start, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == start {
				return fs.SkipAll
			}
			return err
		}
		if strings.HasPrefix(entry.Name(), ".") && path != start {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(f.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, File{Key: key, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Key < files[j].Key })
	return files, nil
}
