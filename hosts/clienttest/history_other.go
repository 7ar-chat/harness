//go:build !unix

package clienttest

import "testing"

func testUndoFiles(t *testing.T, _ Factory) { t.Skip("needs the local sandbox backend (unix)") }
