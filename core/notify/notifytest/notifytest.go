// Package notifytest is the conformance suite every notify.Store passes.
package notifytest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/7ar-chat/harness/core/notify"
)

// Run exercises a store; fresh returns an empty one each call.
func Run(t *testing.T, fresh func(t *testing.T) notify.Store) {
	ctx := context.Background()
	t.Run("rules default and round trip", func(t *testing.T) {
		s := fresh(t)
		r, err := s.GetRules(ctx, "p1")
		if err != nil || r.ProjectID != "p1" || r.DedupeMinutes != notify.DefaultDedupeMinutes || len(r.Events) != 0 {
			t.Fatalf("default rules = %+v, %v", r, err)
		}
		r.Events = map[notify.Kind]notify.Rule{notify.TaskBlocked: {Channels: []string{"c1"}, Person: false}}
		r.Digest, r.DedupeMinutes, r.UpdatedBy = true, 0, "ann"
		if err := s.PutRules(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetRules(ctx, "p1")
		if !got.Digest || got.DedupeMinutes != 0 || got.UpdatedBy != "ann" || got.UpdatedAt.IsZero() || got.RuleFor(notify.TaskBlocked).Person || got.RuleFor(notify.TaskBlocked).Channels[0] != "c1" {
			t.Fatalf("rules = %+v", got)
		}
		if other, _ := s.GetRules(ctx, "p2"); other.Digest {
			t.Fatal("rules leaked across projects")
		}
	})
	t.Run("channels", func(t *testing.T) {
		s := fresh(t)
		if _, err := s.GetChannel(ctx, "p1", "x"); !errors.Is(err, notify.ErrNotFound) {
			t.Fatalf("missing = %v", err)
		}
		for _, c := range []notify.Channel{
			{ID: "b", ProjectID: "p1", Kind: notify.KindWebhook, Name: "Zeta", Enabled: true, Sealed: []byte("s1")},
			{ID: "a", ProjectID: "p1", Kind: notify.KindDiscord, Name: "alpha", Hint: "discord.com"},
			{ID: "c", ProjectID: "p2", Kind: notify.KindWeCom, Name: "Other"},
		} {
			if err := s.PutChannel(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		list, _ := s.ListChannels(ctx, "p1")
		if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" || string(list[1].Sealed) != "s1" || list[0].CreatedAt.IsZero() {
			t.Fatalf("list = %+v", list)
		}
		if _, err := s.GetChannel(ctx, "p2", "a"); !errors.Is(err, notify.ErrNotFound) {
			t.Fatal("a channel is readable through another project")
		}
		if err := s.DeleteChannel(ctx, "p2", "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetChannel(ctx, "p1", "a"); err != nil {
			t.Fatal("deleting through another project removed it")
		}
		_ = s.DeleteChannel(ctx, "p1", "a")
		if err := s.DeleteChannel(ctx, "p1", "a"); err != nil {
			t.Fatal("deleting an absent channel is not an error")
		}
		list, _ = s.ListChannels(ctx, "p1")
		if len(list) != 1 {
			t.Fatalf("after delete = %+v", list)
		}
	})
	t.Run("prefs", func(t *testing.T) {
		s := fresh(t)
		p, err := s.GetPrefs(ctx, "bob")
		if err != nil || p.Login != "bob" || len(p.Events) != 0 {
			t.Fatalf("default prefs = %+v, %v", p, err)
		}
		p.Events = map[notify.Kind][]notify.Via{notify.TaskDone: {notify.Email}}
		p.QuietStart, p.QuietEnd, p.Timezone, p.Digest = "22:00", "07:00", "Europe/Paris", true
		p.SealedWebhook, p.WebhookHint = []byte("w"), "example.com"
		if err := s.PutPrefs(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetPrefs(ctx, "bob")
		if got.QuietStart != "22:00" || got.Timezone != "Europe/Paris" || !got.Digest || got.Events[notify.TaskDone][0] != notify.Email || string(got.SealedWebhook) != "w" {
			t.Fatalf("prefs = %+v", got)
		}
		if other, _ := s.GetPrefs(ctx, "cy"); other.Digest || other.QuietStart != "" {
			t.Fatal("prefs leaked across people")
		}
	})
	t.Run("log keeps the newest and filters", func(t *testing.T) {
		s := fresh(t)
		for i := 0; i < notify.MaxLog+20; i++ {
			login := "ann"
			if i%2 == 1 {
				login = "bob"
			}
			if err := s.AppendLog(ctx, notify.LogEntry{ID: fmt.Sprint(i), ProjectID: "p1", Login: login, Event: notify.TaskBlocked, Title: "t", Channel: "c", Status: notify.Delivered}); err != nil {
				t.Fatal(err)
			}
		}
		_ = s.AppendLog(ctx, notify.LogEntry{ID: "solo", Login: "ann", Event: notify.TaskBlocked, Status: notify.Failed, Error: "boom"})
		list, _ := s.ListLog(ctx, "p1", "", 100)
		if len(list) != 100 || list[0].ID != fmt.Sprint(notify.MaxLog+19) {
			t.Fatalf("newest first: len %d first %q", len(list), list[0].ID)
		}
		all, _ := s.ListLog(ctx, "p1", "", 0)
		if len(all) != notify.MaxLog {
			t.Fatalf("kept %d", len(all))
		}
		mine, _ := s.ListLog(ctx, "p1", "bob", 10)
		for _, e := range mine {
			if e.Login != "bob" {
				t.Fatal("login filter")
			}
		}
		solo, _ := s.ListLog(ctx, "", "ann", 10)
		if len(solo) != 1 || solo[0].Error != "boom" {
			t.Fatalf("project-less log = %+v", solo)
		}
	})
	t.Run("delete project", func(t *testing.T) {
		s := fresh(t)
		_ = s.PutChannel(ctx, notify.Channel{ID: "a", ProjectID: "p1", Name: "n"})
		_ = s.PutRules(ctx, notify.Rules{ProjectID: "p1", Digest: true})
		_ = s.AppendLog(ctx, notify.LogEntry{ID: "1", ProjectID: "p1"})
		_ = s.PutChannel(ctx, notify.Channel{ID: "b", ProjectID: "p2", Name: "n"})
		if err := s.DeleteProject(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.ListChannels(ctx, "p1"); len(l) != 0 {
			t.Fatal("channels survived")
		}
		if r, _ := s.GetRules(ctx, "p1"); r.Digest {
			t.Fatal("rules survived")
		}
		if l, _ := s.ListLog(ctx, "p1", "", 10); len(l) != 0 {
			t.Fatal("log survived")
		}
		if l, _ := s.ListChannels(ctx, "p2"); len(l) != 1 {
			t.Fatal("another project lost its channel")
		}
	})
}
