package sandboxtools_test

import (
	"context"
	"testing"
)

func TestWriteFileCreatesFileAndReportsBytes(t *testing.T) {
	// write_file writes the content (parents created) and returns the absolute path and size.
	// Arrange
	f := newFixture(t, nil)

	// Act
	out, err := f.call(context.Background(), "write_file", `{"path":"scripts/../out/run.py","content":"print(1)\n"}`)

	// Assert
	got, ok := f.sb.File("/workspace/out/run.py")
	if err != nil || out != `{"path":"/workspace/out/run.py","bytesWritten":9}` || !ok || got != "print(1)\n" || !f.sb.HasDir("/workspace/out") {
		t.Fatalf("err %v out %s file %q", err, out, got)
	}
}
