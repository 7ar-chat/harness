package product

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// projectTemplates are the starting points a project can be created from. Each directory under templates/ holds a
// template.json (what the picker shows) and the files copied into the new project, including .har/project.yaml,
// which says how to install and run it (see Manifest).
//
//go:embed all:templates
var projectTemplates embed.FS

// Template is what the project picker shows.
type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Order       int      `json:"order,omitempty"`
}

// TemplateFile is one file of a template, relative to the project root.
type TemplateFile struct {
	Path string
	Data []byte
}

// ManifestPath is where a project describes how to run itself.
const ManifestPath = ".har/project.yaml"

// Manifest is .har/project.yaml: the install and run commands of a project and the port its server listens on.
// The agent may edit it; the preview reads it.
type Manifest struct {
	Template string `yaml:"template,omitempty" json:"template,omitempty"`
	Install  string `yaml:"install,omitempty" json:"install,omitempty"`
	Run      string `yaml:"run" json:"run"`
	Port     int    `yaml:"port" json:"port"`
	// Base is "app" when the server serves itself under $PREVIEW_BASE, "strip" (the default) when the preview proxy
	// removes the path prefix before forwarding.
	Base string `yaml:"base,omitempty" json:"base,omitempty"`
}

// ParseManifest reads and validates a project.yaml.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	m.Run = strings.TrimSpace(m.Run)
	if m.Run == "" {
		return m, errors.New(ManifestPath + ": run is empty")
	}
	if m.Port < 1 || m.Port > 65535 {
		return m, errors.New(ManifestPath + ": port must be between 1 and 65535")
	}
	switch m.Base {
	case "":
		m.Base = "strip"
	case "app", "strip":
	default:
		return m, errors.New(ManifestPath + `: base must be "app" or "strip"`)
	}
	return m, nil
}

var (
	templatesOnce sync.Once
	templateList  []Template
)

// Templates lists the templates in display order.
func Templates() []Template {
	templatesOnce.Do(func() {
		entries, err := fs.ReadDir(projectTemplates, "templates")
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			raw, err := fs.ReadFile(projectTemplates, path.Join("templates", e.Name(), "template.json"))
			if err != nil {
				continue
			}
			var t Template
			if json.Unmarshal(raw, &t) != nil || t.ID != e.Name() {
				continue
			}
			templateList = append(templateList, t)
		}
		sort.SliceStable(templateList, func(i, j int) bool { return templateList[i].Order < templateList[j].Order })
	})
	return append([]Template(nil), templateList...)
}

// TemplateByID reports whether a template exists.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Templates() {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// TemplateFiles returns the files of a template (without template.json).
func TemplateFiles(id string) ([]TemplateFile, error) {
	if _, ok := TemplateByID(id); !ok {
		return nil, fmt.Errorf("unknown template %q", id)
	}
	root := path.Join("templates", id)
	var out []TemplateFile
	err := fs.WalkDir(projectTemplates, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		if rel == "template.json" {
			return nil
		}
		data, err := fs.ReadFile(projectTemplates, p)
		if err != nil {
			return err
		}
		out = append(out, TemplateFile{Path: rel, Data: data})
		return nil
	})
	return out, err
}

var _ embed.FS
