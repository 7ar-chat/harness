// Package layout names the folders of a harness configuration directory
// (<repo>/.har/ and ~/.har/). It is the one place those names are defined: the
// config package re-exports them, and packages that must not import config
// (engine, adapters, features) use them from here.
package layout

// DirName is the name of the configuration directory of a project
// (<repo>/.har/) and of the user (~/.har/). Every path below it derives from it.
const DirName = ".har"

// Folders inside a configuration directory.
const (
	SkillsFolder = "skills"
	MemoryFolder = "memory"
)
