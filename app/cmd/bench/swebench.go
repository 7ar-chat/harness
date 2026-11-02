package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/7ar-chat/harness/bench"
	"github.com/7ar-chat/harness/bench/swebench"
)

// swebenchTasks loads a SWE-bench style JSONL file and materialises its instances (ids narrows them).
func swebenchTasks(path, ids string) ([]bench.Task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	insts, err := swebench.Load(f)
	if err != nil {
		return nil, err
	}
	if ids = strings.TrimSpace(ids); ids != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(ids, ",") {
			want[strings.TrimSpace(id)] = true
		}
		kept := insts[:0]
		for _, in := range insts {
			if want[in.ID] {
				kept = append(kept, in)
				delete(want, in.ID)
			}
		}
		if len(want) > 0 {
			return nil, fmt.Errorf("unknown instance ids %v in %s", keys(want), path)
		}
		insts = kept
	}
	if len(insts) == 0 {
		return nil, fmt.Errorf("%s holds no instances", path)
	}
	return swebench.Suite(context.Background(), insts, swebench.Options{})
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
