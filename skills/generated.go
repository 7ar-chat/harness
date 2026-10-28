package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness/store"
)

// Generated packages of guided skill creation (Work's
// _collect_skill_creation_packages): /workspace/generated-skill holds one
// package per child directory (<skill-name>/SKILL.md), or a single legacy
// package with SKILL.md at its root.
const (
	MaxGeneratedRoots   = 100
	MaxGeneratedEntries = 500
)

// GeneratedFile is a file found under the generated folder.
type GeneratedFile struct {
	// Path is absolute (/workspace/generated-skill/...).
	Path string
	Dir  bool
	Size int64
}

// GeneratedSource reads the session's workspace (live sandbox or saved artifacts).
type GeneratedSource interface {
	// List returns every entry under root, recursively (store.ErrNotFound when root is missing).
	List(ctx context.Context, root string) ([]GeneratedFile, error)
	// Read returns at most maxBytes+1 bytes of a file.
	Read(ctx context.Context, path string, maxBytes int64) ([]byte, error)
}

// GeneratedError is a package collection failure (Work's 422 detail).
type GeneratedError struct{ Detail string }

func (e *GeneratedError) Error() string { return e.Detail }

func generatedErr(format string, args ...any) error {
	return &GeneratedError{Detail: fmt.Sprintf(format, args...)}
}

// GeneratedPackage is one collected package.
type GeneratedPackage struct {
	// Root is relative to /workspace (generated-skill or generated-skill/<name>).
	Root     string
	Revision string
	Size     int64
	Files    []File
}

// GeneratedRevision is the batch revision (Work's _skill_packages_revision).
func GeneratedRevision(pkgs []GeneratedPackage) string {
	digest := sha256.New()
	for _, p := range pkgs {
		digest.Write([]byte(p.Root + "\x00" + p.Revision + "\x00"))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

const workspacePrefix = "/workspace/"

func rel(p string) string { return strings.TrimPrefix(path.Clean(p), workspacePrefix) }

// CollectGenerated collects the generated packages under root (absolute,
// e.g. /workspace/generated-skill); none is an empty result.
func CollectGenerated(ctx context.Context, src GeneratedSource, root string) ([]GeneratedPackage, error) {
	root = path.Clean(root)
	entries, err := src.List(ctx, root)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var roots []string
	hasRootSkill := false
	var stray []string
	for _, e := range entries {
		p := path.Clean(e.Path)
		if path.Dir(p) != root {
			continue
		}
		switch {
		case !e.Dir && path.Base(p) == "SKILL.md":
			hasRootSkill = true
		case e.Dir:
			roots = append(roots, p)
		default:
			stray = append(stray, p)
		}
	}
	if hasRootSkill {
		roots = []string{root}
	} else {
		sort.Strings(stray)
		if len(stray) > 0 {
			return nil, generatedErr("Every entry under the generated Skill folder must be a skill directory. Unexpected file: %s.", stray[0])
		}
		if len(roots) == 0 {
			return nil, nil
		}
	}
	sort.Strings(roots)
	if len(roots) > MaxGeneratedRoots {
		return nil, generatedErr("The generated Skill folder contains more than %d skill packages.", MaxGeneratedRoots)
	}
	var out []GeneratedPackage
	var batch int64
	for _, r := range roots {
		pkg, err := collectRoot(ctx, src, r, entries, MaxDraftBatchBytes-batch)
		if err != nil {
			return nil, err
		}
		if r == root {
			for _, f := range pkg.Files {
				if path.Base(f.Path) == "SKILL.md" && strings.Count(f.Path, "/") == 1 {
					return nil, generatedErr("The generated Skill folder cannot mix the legacy root SKILL.md layout with child skill directories.")
				}
			}
		}
		out = append(out, pkg)
		batch += pkg.Size
	}
	return out, nil
}

func collectRoot(ctx context.Context, src GeneratedSource, root string, entries []GeneratedFile, remaining int64) (GeneratedPackage, error) {
	var files []GeneratedFile
	count := 0
	tooLarge := func(p string, limit int64) error {
		return generatedErr("The generated file %s exceeds the %d bytes file size limit.", p, limit)
	}
	var declared int64
	for _, e := range entries {
		p := path.Clean(e.Path)
		if !strings.HasPrefix(p, root+"/") {
			continue
		}
		count++
		if count > MaxGeneratedEntries {
			return GeneratedPackage{}, generatedErr("The generated skill package at %s contains too many entries.", root)
		}
		if e.Dir {
			continue
		}
		limit := int64(MaxFileBytes)
		if p == root+"/SKILL.md" {
			limit = MaxSkillMDBytes
		}
		if e.Size > limit {
			return GeneratedPackage{}, tooLarge(p, limit)
		}
		declared += e.Size
		if declared > MaxPackageBytes || declared > remaining {
			return GeneratedPackage{}, generatedErr("The generated Skill packages exceed the %d bytes total size limit.", MaxDraftBatchBytes)
		}
		files = append(files, GeneratedFile{Path: p, Size: e.Size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	hasSkill := false
	for _, f := range files {
		hasSkill = hasSkill || f.Path == root+"/SKILL.md"
	}
	if !hasSkill {
		return GeneratedPackage{}, generatedErr("The generated Skill package must contain %s/SKILL.md.", root)
	}
	pkg := GeneratedPackage{Root: rel(root)}
	digest := sha256.New()
	for _, f := range files {
		limit := int64(MaxFileBytes)
		if f.Path == root+"/SKILL.md" {
			limit = MaxSkillMDBytes
		}
		data, err := src.Read(ctx, f.Path, limit)
		if errors.Is(err, store.ErrNotFound) {
			continue // removed while collecting
		}
		if err != nil {
			return GeneratedPackage{}, err
		}
		if int64(len(data)) > limit {
			return GeneratedPackage{}, tooLarge(f.Path, limit)
		}
		pkg.Size += int64(len(data))
		if pkg.Size > MaxPackageBytes || pkg.Size > remaining {
			return GeneratedPackage{}, generatedErr("The generated Skill packages exceed the %d bytes total size limit.", MaxDraftBatchBytes)
		}
		relPath := strings.TrimPrefix(f.Path, root+"/")
		digest.Write([]byte(relPath))
		digest.Write([]byte{0})
		digest.Write(data)
		digest.Write([]byte{0})
		pkg.Files = append(pkg.Files, File{Path: relPath, Data: data})
	}
	pkg.Revision = hex.EncodeToString(digest.Sum(nil))
	return pkg, nil
}

// GeneratedSkill summarizes a ready package.
type GeneratedSkill struct {
	Root        string
	Name        string
	Description string
}

// GeneratedStatus is the package-status view (Work's
// CoworkSkillCreationPackageStatus): empty, invalid (with issues) or ready.
type GeneratedStatus struct {
	State      string
	Revision   string
	SkillCount int
	Skills     []GeneratedSkill
	Issues     []Issue
	// Packages are the validated packages (ready only).
	Packages []DraftPackage
}

// Package states.
const (
	StateEmpty   = "empty"
	StateInvalid = "invalid"
	StateReady   = "ready"
)

// InspectGenerated collects and validates the generated packages without
// failing on validation errors (Work's get_skill_creation_package_status).
func InspectGenerated(ctx context.Context, src GeneratedSource, root string) (GeneratedStatus, error) {
	pkgs, err := CollectGenerated(ctx, src, root)
	var gerr *GeneratedError
	if errors.As(err, &gerr) {
		return GeneratedStatus{State: StateInvalid, Revision: StateInvalid, Skills: []GeneratedSkill{},
			Issues: []Issue{{Code: "invalid_package", Message: gerr.Detail, Severity: SeverityError}}}, nil
	}
	if err != nil {
		return GeneratedStatus{}, err
	}
	if len(pkgs) == 0 {
		return GeneratedStatus{State: StateEmpty, Revision: StateEmpty, Skills: []GeneratedSkill{}, Issues: []Issue{}}, nil
	}
	st := GeneratedStatus{State: StateReady, Revision: GeneratedRevision(pkgs), SkillCount: len(pkgs), Skills: []GeneratedSkill{}, Issues: []Issue{}}
	for _, p := range pkgs {
		v := ValidateFiles(p.Files)
		if !v.Valid {
			st.State = StateInvalid
			for _, i := range v.Issues {
				i.Message = workspacePrefix + p.Root + "/" + firstNonEmpty(i.Path, "SKILL.md") + ": " + i.Message
				i.Path = workspacePrefix + p.Root + "/" + firstNonEmpty(i.Path, "SKILL.md")
				st.Issues = append(st.Issues, i)
			}
			continue
		}
		st.Skills = append(st.Skills, GeneratedSkill{Root: p.Root, Name: v.Package.Name, Description: v.Package.Description})
		st.Packages = append(st.Packages, DraftPackage{Package: v.Package, SourceType: store.SkillSourceGenerated, Root: p.Root})
	}
	if st.State != StateReady {
		st.Packages = nil
	}
	return st, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
