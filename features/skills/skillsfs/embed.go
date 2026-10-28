// Package skillsfs embeds the built-in skills (builtin/<name>/).
package skillsfs

import (
	"embed"
	"io/fs"
)

// FS holds builtin/<name>/SKILL.md and the skills' files. Only the skill
// folders are listed; embed_test.go fails when a builtin/<name>/SKILL.md
// folder is missing here.
//
//go:embed all:builtin/charts all:builtin/docx all:builtin/pdf all:builtin/pptx all:builtin/xlsx
var FS embed.FS

// Builtin returns the builtin directory (one sub-directory per skill).
func Builtin() fs.FS {
	sub, err := fs.Sub(FS, "builtin")
	if err != nil {
		return nil
	}
	return sub
}
