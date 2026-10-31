package pgstore_test

import (
	"context"
	"fmt"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/core/store"
)

func TestPGMigrateUpgradesDatabaseAtTheSecondMigration(t *testing.T) {
	// A database with only 0001 + 0002 applied gets both harness v2 migrations (artifacts/skills, harness) without conflicts.
	// Arrange
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_v1_%d", time.Now().UnixNano())
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
	stmts := []string{"CREATE SCHEMA " + schema,
		"CREATE TABLE work_migrations (id SERIAL PRIMARY KEY, name VARCHAR(255) NOT NULL UNIQUE, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())"}
	for _, name := range []string{"0001_initial.sql", "0002_run_error_code.sql"} {
		sql, err := os.ReadFile("migrate/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, string(sql), "INSERT INTO work_migrations (name) VALUES ('"+name+"')")
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// v1 data: a titled thread with a finished run.
	threadID, runID := uuid.NewString(), uuid.NewString()
	for _, q := range []string{
		"INSERT INTO work_threads (id, user_login, title) VALUES ('" + threadID + "', 'alice', 'before the upgrade')",
		"INSERT INTO work_runs (id, thread_id, user_login, status, model, userinfo) VALUES ('" + runID + "', '" + threadID + "', 'alice', 'completed', 'm', 'ui')",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s := pgstore.NewPG(pool)

	// Act
	first := migrate.Run(ctx, pool, schema)
	second := migrate.Run(ctx, pool, schema)

	// Assert
	if first != nil || second != nil {
		t.Fatalf("first %v second %v", first, second)
	}
	var cols []string
	rows, _ := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'work_runs'
		AND column_name IN ('skills', 'kind', 'reasoning_effort') ORDER BY column_name`, schema)
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if fmt.Sprint(cols) != "[kind reasoning_effort skills]" {
		t.Fatalf("work_runs columns %v", cols)
	}
	old, oldErr := s.GetRun(ctx, runID)
	in := newRun(threadID, "after the upgrade")
	in.Skills, in.ReasoningEffort = []string{"docx"}, "high"
	run, _, err := s.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	skills, _ := s.RunSkills(ctx, run.ID)
	got, _ := s.GetThread(ctx, "alice", threadID)
	if oldErr != nil || old.Kind != store.KindChat || run.Kind != store.KindChat || run.ReasoningEffort != "high" || fmt.Sprint(skills) != "[docx]" || got.Title != "before the upgrade" || got.TitleSource != "user" {
		t.Fatalf("run %+v skills %v thread %+v", run, skills, got)
	}
}

func TestPGResumeRunWithSkillsAndAttachments(t *testing.T) {
	// A run answering a question can carry requested skills and an attachments-only message; all are stored.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	_ = s.SetPendingQuestion(ctx, th.ID, &store.PendingQuestion{InterruptID: "i1", ToolCallID: "call_ask", Question: "Which file?", AllowFreeText: true})
	in := newRun(th.ID, "")
	in.Message.Metadata = map[string]any{"attachments": []any{map[string]any{"artifactId": "a1", "name": "d.csv", "path": "/workspace/uploads/d.csv",
		"sizeBytes": 3, "mimeType": "text/csv", "version": 1}}}
	in.Resume = &store.Resume{InterruptID: "i1", Content: "The user answered: this one"}
	in.Skills = []string{"xlsx"}

	// Act
	run, msg, err := s.CreateRun(ctx, in)

	// Assert
	if err != nil || msg == nil {
		t.Fatalf("err %v msg %+v", err, msg)
	}
	skills, _ := s.RunSkills(ctx, run.ID)
	msgs, _ := s.ListMessages(ctx, th.ID)
	if fmt.Sprint(skills) != "[xlsx]" || len(msgs) != 2 || msgs[0].Role != store.RoleTool || msgs[0].ToolCallID != "call_ask" ||
		len(store.MessageAttachments(msgs[1])) != 1 || store.MessageAttachments(msgs[1])[0].Path != "/workspace/uploads/d.csv" {
		t.Fatalf("skills %v msgs %+v", skills, msgs)
	}
}

func TestPGAppendRunArtifactsKeepsArtifactsAfterCompaction(t *testing.T) {
	// Persisting a compaction summary never touches messages: metadata.artifacts of tool and assistant messages survive.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	run, user, _ := s.CreateRun(ctx, newRun(th.ID, "build"))
	_ = s.AppendMessages(ctx,
		&store.Message{ID: "a1", ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{
			{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "execute_command", Arguments: `{}`}}}},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "ok",
			Metadata: map[string]any{"artifacts": []any{map[string]any{"artifactId": "x1", "path": "/workspace/r.docx", "name": "r.docx", "version": 1, "change": "created"}}}},
		&store.Message{ID: "a2", ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "done"},
	)
	_ = s.AppendRunArtifacts(ctx, run.ID, []any{map[string]any{"artifactId": "x2", "path": "/workspace/late.csv", "name": "late.csv", "version": 1, "change": "created"}})

	// Act
	err := s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: user.Position + 3})

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	if err != nil || len(msgs) != 4 || fmt.Sprint(store.ArtifactPaths(msgs[2].Metadata)) != "[/workspace/r.docx]" ||
		fmt.Sprint(store.ArtifactPaths(msgs[3].Metadata)) != "[/workspace/late.csv]" {
		t.Fatalf("err %v msgs %+v", err, msgs)
	}
}

func TestPGMigrate0005GivesUntitledThreadsAutoTitlesAndTracksLiveArtifacts(t *testing.T) {
	// Upgrading a database at 0004: threads that predate 0004 without a title get title_source auto, live artifacts are materialized in the thread's sandbox.
	// Arrange
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_v4_%d", time.Now().UnixNano())
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); pool.Close() })
	untitled, titled, artifact := uuid.NewString(), uuid.NewString(), uuid.NewString()
	stmts := []string{"CREATE SCHEMA " + schema,
		"CREATE TABLE work_migrations (id SERIAL PRIMARY KEY, name VARCHAR(255) NOT NULL UNIQUE, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())"}
	for _, name := range []string{"0001_initial.sql", "0002_run_error_code.sql", "0003_artifacts_skills.sql"} {
		sql, err := os.ReadFile("migrate/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, string(sql), "INSERT INTO work_migrations (name) VALUES ('"+name+"')")
	}
	stmts = append(stmts,
		"INSERT INTO work_threads (id, user_login, title, sandbox_id) VALUES ('"+untitled+"', 'alice', '', 'sb-1'), ('"+titled+"', 'alice', 'Budget', NULL)",
		"INSERT INTO work_artifacts (id, thread_id, user_login, path, name, mime_type, kind, size_bytes, sha256, version) VALUES ('"+artifact+"', '"+untitled+"', 'alice', '/workspace/a', 'a', 'text/plain', 'output', 1, 'x', 1)")
	sql4, _ := os.ReadFile("migrate/migrations/0004_harness.sql")
	stmts = append(stmts, string(sql4), "INSERT INTO work_migrations (name) VALUES ('0004_harness.sql')")
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s := pgstore.NewPG(pool)

	// Act
	first := migrate.Run(ctx, pool, schema)
	second := migrate.Run(ctx, pool, schema)

	// Assert
	a, _ := s.GetThread(ctx, "alice", untitled)
	b, _ := s.GetThread(ctx, "alice", titled)
	art, _ := s.GetArtifact(ctx, artifact)
	if first != nil || second != nil || a.TitleSource != store.TitleAuto || b.TitleSource != store.TitleUser || art.SandboxID != "sb-1" {
		t.Fatalf("migrate %v %v untitled %+v titled %+v artifact %+v", first, second, a, b, art)
	}
}
