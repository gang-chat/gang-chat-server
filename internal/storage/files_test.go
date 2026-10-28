package storage

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func keys(files []File) []string {
	out := []string{}
	for _, file := range files {
		out = append(out, file.Key)
	}
	return out
}

func TestFilesStoreReadListAndDelete(t *testing.T) {
	files, err := NewFiles(filepath.Join(t.TempDir(), "storage"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"assets/ast_1/a.png", "assets/releases/latest.yml", "assets/releases/GangChat_v1.0.0.exe", "other.txt"} {
		if err := files.Put(key, strings.NewReader("body of "+key)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	// A Put that never finished leaves only a temporary file, which nobody sees.
	if err := os.WriteFile(filepath.Join(files.Root(), "assets", "releases", tempPrefix+"123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, info, err := files.Open("assets/releases/latest.yml")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(file)
	file.Close()
	if string(raw) != "body of assets/releases/latest.yml" || info.Size() != int64(len(raw)) {
		t.Fatalf("read %q", raw)
	}

	all, err := files.List("")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"assets/ast_1/a.png", "assets/releases/GangChat_v1.0.0.exe", "assets/releases/latest.yml", "other.txt"}
	if got := keys(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("list: %v", got)
	}
	releases, err := files.List(ReleasePrefix + "Gang")
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(releases); !reflect.DeepEqual(got, []string{"assets/releases/GangChat_v1.0.0.exe"}) {
		t.Fatalf("list by prefix: %v", got)
	}
	if missing, err := files.List("nothing/here/"); err != nil || len(missing) != 0 {
		t.Fatalf("a missing folder lists nothing: %v %v", missing, err)
	}

	if err := files.Delete("assets/ast_1/a.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(files.Root(), "assets", "ast_1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("deleting an asset's last file must remove its folder")
	}
	if _, err := os.Stat(filepath.Join(files.Root(), "assets")); err != nil {
		t.Fatal("folders that still hold files must stay")
	}
	if err := files.Delete("assets/ast_1/a.png"); err != nil {
		t.Fatalf("deleting a missing file is not an error: %v", err)
	}
	if _, _, err := files.Open("assets/ast_1/a.png"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a deleted file must read as missing: %v", err)
	}
	if _, _, err := files.Open("assets/releases"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a folder is not a file: %v", err)
	}
}

func TestFilesReplaceAFileWhole(t *testing.T) {
	files, err := NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Put("assets/releases/latest.yml", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	reader, _, err := files.Open("assets/releases/latest.yml")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := files.Put("assets/releases/latest.yml", strings.NewReader("new")); err != nil {
		t.Fatal(err)
	}
	// A download that started before the replacement keeps reading the old file.
	if raw, _ := io.ReadAll(reader); string(raw) != "old" {
		t.Fatalf("open file changed under its reader: %q", raw)
	}
	if file, err := files.Stat("assets/releases/latest.yml"); err != nil || file.Size != 3 {
		t.Fatalf("stat: %+v %v", file, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(files.Root(), "assets", "releases")); len(entries) != 1 {
		t.Fatalf("temporary files must not stay behind: %v", entries)
	}
}

func TestFilesRefuseKeysOutsideTheRoot(t *testing.T) {
	files, err := NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "/etc/passwd", "../up", "assets/../../up", "assets//a", "assets/", ".hidden", "assets/.put-1", `assets\a`, "a\x00b"} {
		if err := files.Put(key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("put %q: %v", key, err)
		}
		if _, _, err := files.Open(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("open %q: %v", key, err)
		}
	}
	if _, err := NewFiles(" "); err == nil {
		t.Fatal("an empty storage path must be refused")
	}
}
