package codesearch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestShellFields(t *testing.T) {
	fields, err := shellFields(`-Fi "SYSTEM_PROMPT" -d recurse`)
	if err != nil {
		t.Fatalf("shellFields returned error: %v", err)
	}
	want := []string{"-Fi", "SYSTEM_PROMPT", "-d", "recurse"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
}

func TestSearchExcludesEnvFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET_MARKER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("SECRET_MARKER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	searcher, err := New(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	output, err := searcher.Search("-r -n SECRET_MARKER")
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if output != "./source.go:1:SECRET_MARKER" {
		t.Fatalf("output = %q, want only source.go match", output)
	}
}

func TestSearchIncludesGrepErrorOutput(t *testing.T) {
	searcher, err := New(t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = searcher.Search("-r -n SECRET_MARKER missing-file")
	if err == nil {
		t.Fatal("Search returned nil error")
	}
	if got := err.Error(); got != "grep: missing-file: No such file or directory" {
		t.Fatalf("error = %q, want grep stderr", got)
	}
}

func TestUniqueLinesRemovesDuplicateDiagnostics(t *testing.T) {
	got := uniqueLines("grep: .: Is a directory\ngrep: .: Is a directory")
	if got != "grep: .: Is a directory" {
		t.Fatalf("message = %q", got)
	}
}
