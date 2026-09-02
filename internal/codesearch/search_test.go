package codesearch

import (
	"reflect"
	"testing"
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
