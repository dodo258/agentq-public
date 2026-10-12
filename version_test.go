package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestReleaseVersion(t *testing.T) {
	if releaseVersion() == "" {
		t.Fatal("empty version")
	}
	for _, arg := range []string{"version", "--version", "-V"} {
		var out, err bytes.Buffer
		if code := realMain([]string{arg}, &out, &err); code != 0 || out.String() != "agentq "+releaseVersion()+"\n" || err.Len() != 0 {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q", arg, code, out.String(), err.String())
		}
	}
	var out, err bytes.Buffer
	if realMain([]string{"version", "extra"}, &out, &err) != 2 || !strings.Contains(err.String(), "unexpected arguments") {
		t.Fatal("version accepted arguments")
	}
}
