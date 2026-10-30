package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratedFileIsCurrent(t *testing.T) {
	// Arrange
	want, err := render()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	got, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(outFile)))

	// Assert
	if err != nil {
		t.Fatalf("%v: run `make gen`", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: run `make gen` and commit the result", outFile)
	}
}

func TestGenerationIsDeterministic(t *testing.T) {
	// Arrange / Act
	a, errA := render()
	b, errB := render()

	// Assert
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two runs wrote different files")
	}
}

type inner struct {
	A string `json:"a"`
	B int    `json:"b,omitempty"`
}

type sample struct {
	inner
	Name     string          `json:"name"`
	When     time.Time       `json:"when"`
	Raw      json.RawMessage `json:"raw"`
	Any      map[string]any  `json:"any"`
	Ptr      *inner          `json:"ptr"`
	OptPtr   *inner          `json:"optPtr,omitempty"`
	List     []inner         `json:"list"`
	Bytes    []byte          `json:"bytes"`
	Skipped  string          `json:"-"`
	hidden   string
	Untagged float64
	Shadowed string `json:"a"`
	Nested   struct {
		X []string `json:"x"`
	} `json:"nested"`
	Dash map[string][]*inner `json:"dash,omitempty"`
	Mode string              `json:"mode"`
	Dot  string              `json:"a.b"`
}

func TestGeneratorMapsGoTypesLikeEncodingJSON(t *testing.T) {
	// Arrange
	g := newGenerator([]enum{{"Mode", []string{"x", "y"}}}, nil, map[string]string{"main.sample.mode": "Mode"})

	// Act
	out, err := g.generate([]root{{"Sample", sample{}}, {"Inner", inner{}}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"export interface Sample extends Inner {\n",
		"  name: string;\n",
		"  when: string;\n",
		"  raw: unknown;\n",
		"  any: Record<string, unknown>;\n",
		"  ptr: Inner | null;\n",
		"  optPtr?: Inner;\n",
		"  list: Inner[];\n",
		"  bytes: string;\n",
		"  Untagged: number;\n",
		"  a: string;\n", // the shallower field hides the embedded one
		"  nested: {\n    x: string[];\n  };\n",
		"  dash?: Record<string, (Inner | null)[]>;\n",
		"  mode: Mode;\n",
		"  \"a.b\": string;\n",
		"export type Mode = \"x\" | \"y\";\n",
		"export interface Inner {\n  a: string;\n  b?: number;\n}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Skipped", "hidden"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s must not be generated", bad)
		}
	}
}

func TestGeneratorRejectsWhatItCannotMap(t *testing.T) {
	// Arrange
	type withChan struct {
		C chan int `json:"c"`
	}
	type withIntKeys struct {
		M map[int]string `json:"m"`
	}

	for name, tc := range map[string]struct {
		v         any
		overrides map[string]string
		want      string
	}{
		"channel":         {withChan{}, nil, "cannot map"},
		"non-string key":  {withIntKeys{}, nil, "not a string"},
		"stale override":  {inner{}, map[string]string{"main.nothing.x": "string"}, "match no field"},
		"unnamed type":    {sample{}, nil, "is unexported"},
		"non-struct root": {0, nil, "not a struct"},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := newGenerator(nil, nil, tc.overrides).generate([]root{{"T", tc.v}})

			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
