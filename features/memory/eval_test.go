package memory_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/features/memory"
)

// A small retrieval benchmark in the spirit of LongMemEval: memories that a team would keep, and
// questions asked in different words. It measures recall@1, recall@3 and MRR of memory_search, for the
// word-based ranking alone and with embeddings, so a change to ranking is judged by numbers.
//
//	go test ./features/memory -run Eval -v
//
// With HARNESS_EVAL_EMBEDDING_URL (and _KEY, _MODEL) set, it also reports a real embedding model.

type evalEntry struct{ label, content string }
type evalQuery struct {
	q, want string
	para    bool // asked in other words than the memory's
}

var evalEntries = []evalEntry{
	{"deploy", "Deploys go through staging first, then production, from the main branch."},
	{"ci", "Every change needs green CI and at least 80% test coverage before it is merged."},
	{"style", "Go code is formatted with gofmt and checked with golangci-lint; names follow Effective Go."},
	{"db", "Schema changes are Postgres migrations in db/migrations, never edited after merge."},
	{"auth", "Login is OpenID Connect; browser sessions are HttpOnly cookies signed with the session key."},
	{"secrets", "Secrets live in the vault; nothing sensitive is committed or put in memory."},
	{"oncall", "The on-call rotation changes on Mondays at 09:00 and the runbook is in the wiki."},
	{"branches", "Branches are named feature/<ticket> and squashed on merge."},
	{"logging", "Services log JSON lines to stdout with a request id on every line."},
	{"cache", "Redis caches product pages for five minutes; invalidate by publishing on the products channel."},
}

var evalQueries = []evalQuery{
	{"how do we release to production?", "deploy", true},
	{"which environment comes before prod when we ship?", "deploy", true},
	{"deploys to staging", "deploy", false},
	{"what must pass before a pull request can be merged?", "ci", true},
	{"minimum test coverage", "ci", false},
	{"how should I lint and format the code?", "style", true},
	{"gofmt golangci-lint", "style", false},
	{"how do I change the database structure?", "db", true},
	{"postgres migrations", "db", false},
	{"how do users sign in?", "auth", true},
	{"where are cookies signed", "auth", false},
	{"where do passwords and API keys go?", "secrets", true},
	{"who is on call this week and when does it rotate?", "oncall", false},
	{"how do I name a git branch?", "branches", true},
	{"what format are the logs in?", "logging", true},
	{"how long are product pages cached?", "cache", false},
}

type evalResult struct{ r1, r3, mrr, paraR3 float64 }

func runEval(t *testing.T, emb memory.Embedder) evalResult {
	t.Helper()
	f := newFixture(memory.Options{Embedder: emb})
	ids := map[string]string{} // path → label
	for _, e := range evalEntries {
		r := f.svc.Create(context.Background(), f.w, memory.CreateInput{Content: e.content, Title: ptr(e.label)})
		if r.IsError {
			t.Fatal(r.Data)
		}
		ids[r.Data["memory"].(map[string]any)["path"].(string)] = e.label
	}
	var res evalResult
	var para, paraHit float64
	for _, q := range evalQueries {
		r := f.svc.Search(context.Background(), "alice", memory.SearchInput{Query: q.q, TopK: 5})
		rank := 0
		for i, h := range r.Data["results"].([]map[string]any) {
			if ids[h["path"].(string)] == q.want {
				rank = i + 1
				break
			}
		}
		if rank == 1 {
			res.r1++
		}
		if rank >= 1 && rank <= 3 {
			res.r3++
			if q.para {
				paraHit++
			}
		}
		if rank > 0 {
			res.mrr += 1 / float64(rank)
		}
		if q.para {
			para++
		}
	}
	n := float64(len(evalQueries))
	res.r1, res.r3, res.mrr, res.paraR3 = res.r1/n, res.r3/n, res.mrr/n, paraHit/para
	return res
}

func TestEvalRetrieval(t *testing.T) {
	words := runEval(t, nil)
	t.Logf("words only         recall@1 %.2f  recall@3 %.2f  MRR %.2f  paraphrases recall@3 %.2f", words.r1, words.r3, words.mrr, words.paraR3)
	emb := runEval(t, &topicEmbedder{})
	t.Logf("topic embedder     recall@1 %.2f  recall@3 %.2f  MRR %.2f  paraphrases recall@3 %.2f", emb.r1, emb.r3, emb.mrr, emb.paraR3)
	if url := os.Getenv("HARNESS_EVAL_EMBEDDING_URL"); url != "" {
		real := runEval(t, &memory.OpenAIEmbedder{BaseURL: url, APIKey: os.Getenv("HARNESS_EVAL_EMBEDDING_KEY"), ModelName: os.Getenv("HARNESS_EVAL_EMBEDDING_MODEL")})
		t.Logf("real embeddings    recall@1 %.2f  recall@3 %.2f  MRR %.2f  paraphrases recall@3 %.2f", real.r1, real.r3, real.mrr, real.paraR3)
	}
	// Regression floors. The word ranking must keep finding what shares its words; embeddings must not lose to it.
	if words.r3 < 0.55 {
		t.Errorf("word ranking recall@3 fell to %.2f", words.r3)
	}
	if emb.r3 < words.r3 || emb.mrr < words.mrr-0.02 {
		t.Errorf("embeddings (recall@3 %.2f, MRR %.2f) do worse than words alone (%.2f, %.2f)", emb.r3, emb.mrr, words.r3, words.mrr)
	}
	_ = strings.ToLower
	_ = store.MemoryActive
}
