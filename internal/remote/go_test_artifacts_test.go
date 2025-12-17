package remote

import "testing"

func TestExtractGoTestFailureArtifact(t *testing.T) {
	cmd := "go test ./..."
	out := `
=== RUN   TestFoo
--- FAIL: TestFoo (0.01s)
    foo_test.go:12: expected 1, got 2
FAIL	example.com/mod/pkg/foo	0.123s
ok  	example.com/mod/pkg/bar	0.005s
FAIL
`
	title, payload, ok := extractGoTestFailureArtifact(cmd, out)
	if !ok {
		t.Fatalf("expected ok")
	}
	if title == "" {
		t.Fatalf("expected title")
	}
	if len(payload) == 0 {
		t.Fatalf("expected payload")
	}
}
