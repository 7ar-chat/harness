package bench_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/bench"
)

func TestEveryLongTaskFailsBeforeTheFixAndPassesWithTheReferenceSolution(t *testing.T) {
	for _, task := range bench.LongTasks() {
		t.Run(task.Name, func(t *testing.T) {
			// Arrange
			needs(t, task)
			ctx := context.Background()
			before, after := t.TempDir(), t.TempDir()
			if err := bench.Materialize(before, task.Files); err != nil {
				t.Fatal(err)
			}
			if err := bench.Materialize(after, task.Files, task.Solution); err != nil {
				t.Fatal(err)
			}

			// Act
			okBefore, outBefore := bench.Check(ctx, task, before, "")
			okAfter, outAfter := bench.Check(ctx, task, after, task.SolutionAnswer)

			// Assert
			if okBefore {
				t.Errorf("the untouched repository already passes:\n%s", outBefore)
			}
			if !okAfter {
				t.Errorf("the reference solution does not pass:\n%s", outAfter)
			}
		})
	}
}

func TestLongTasksAreBigEnoughToNeedManySteps(t *testing.T) {
	// A long task has many files to touch: 25+ files for the migration, 40 packages to search, 8 stubs to fill.
	min := map[string]int{"go-migrate-api-v2": 25, "go-fix-shared-helper": 40, "go-feature-across-eight-files": 8}
	names := map[string]bool{}
	for _, task := range bench.LongTasks() {
		names[task.Name] = true
		if len(task.Files) < min[task.Name] || len(task.Solution) == 0 || len(task.Hidden) == 0 || task.Verify == "" || task.Prompt == "" {
			t.Errorf("%s: %d files, %d solution, %d hidden", task.Name, len(task.Files), len(task.Solution), len(task.Hidden))
		}
		if !strings.Contains(task.Prompt, "do not ask") {
			t.Errorf("%s: the prompt lets the agent ask questions", task.Name)
		}
	}
	for n := range min {
		if !names[n] {
			t.Errorf("missing task %s", n)
		}
	}
}

func TestMigrationTaskFailsWhenOnlyHalfIsMigrated(t *testing.T) {
	// With v1 removed (Hidden), one forgotten call site breaks the build.
	// Arrange
	var task bench.Task
	for _, tk := range bench.LongTasks() {
		if tk.Name == "go-migrate-api-v2" {
			task = tk
		}
	}
	needs(t, task)
	half := map[string]string{}
	i := 0
	for p, c := range task.Solution {
		if i++; i%2 == 0 {
			half[p] = c
		}
	}
	dir := t.TempDir()
	_ = bench.Materialize(dir, task.Files, half)

	// Act
	ok, out := bench.Check(context.Background(), task, dir, "")

	// Assert
	if ok {
		t.Fatalf("a half-migrated repository passed:\n%s", out)
	}
}
