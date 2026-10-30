package product

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/teamstore"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// replicas starts two web servers over one team store and one bus: two replicas of one deployment.
func replicas(t *testing.T, bus FeedBus) (a, b *rig, project string) {
	t.Helper()
	tm := teamstore.NewMem()
	ctx := context.Background()
	for _, l := range []string{"alice", "bob", "carol"} {
		if _, err := tm.UpsertUser(ctx, store.User{Login: l, Name: l, Email: l + "@x.io"}); err != nil {
			t.Fatal(err)
		}
	}
	a = newRig(t, rigOpts{team: true, prod: Options{Team: tm, Bus: bus}})
	b = newRig(t, rigOpts{team: true, prod: Options{Team: tm, Bus: bus}})
	project = a.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	a.json("PUT", "/api/projects/"+project+"/members/bob", "alice", `{"role":"member"}`, 204)
	return a, b, project
}

// feedOf opens user's live feed on r and returns the data lines it receives.
func feedOf(r *rig, user string) <-chan string {
	resp := r.do("GET", "/api/events", user, "")
	out := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				out <- sc.Text()
			}
		}
	}()
	return out
}

func TestFeedReachesSubscribersOfOtherReplicas(t *testing.T) {
	redisBus := func(t *testing.T) FeedBus {
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })
		return redisx.NewFeedBus(rdb)
	}
	buses := map[string]func(*testing.T) FeedBus{
		"memory": func(*testing.T) FeedBus { return NewMemBus() },
		"redis":  redisBus,
	}
	for name, mk := range buses {
		t.Run(name, func(t *testing.T) {
			// Arrange: bob and carol are connected to replica B, alice (who starts the run) works on replica A.
			a, b, project := replicas(t, mk(t))
			bob, carol, alice := feedOf(b, "bob"), feedOf(b, "carol"), feedOf(a, "alice")
			time.Sleep(300 * time.Millisecond) // the bus subscriptions are in place

			// Act
			th := a.json("POST", "/api/threads", "alice", `{"projectId":"`+project+`"}`, 201)["id"].(string)
			run := a.startRun("alice", th, "go")
			finished(t, a.stream(run, "alice"))

			// Assert: the events produced on A reach bob on B, once and in order; the outsider sees nothing.
			var seen []string
			timeout := time.After(5 * time.Second)
			for !(contains(seen, `"run.started"`) && contains(seen, `"run.finished"`)) {
				select {
				case l := <-bob:
					seen = append(seen, l)
				case <-timeout:
					t.Fatalf("bob's feed on replica B: %v", seen)
				}
			}
			if n := count(seen, `"run.started"`); n != 1 {
				t.Fatalf("run.started reached bob %d times: %v", n, seen)
			}
			select {
			case l := <-carol:
				t.Fatalf("an outsider on replica B saw %s", l)
			case <-time.After(200 * time.Millisecond):
			}
			// A subscriber of the producing replica gets each event once: its own bus echo is dropped.
			var own []string
			for len(own) < 3 {
				select {
				case l := <-alice:
					own = append(own, l)
				case <-time.After(2 * time.Second):
					t.Fatalf("alice's feed on replica A: %v", own)
				}
			}
			select {
			case l := <-alice:
				if count(own, `"run.started"`) > 1 || strings.Contains(l, `"run.started"`) {
					t.Fatalf("an event came back from the bus: %v %s", own, l)
				}
			case <-time.After(200 * time.Millisecond):
			}
			if n := count(own, `"run.started"`); n != 1 {
				t.Fatalf("run.started reached alice %d times: %v", n, own)
			}
		})
	}
}

func contains(lines []string, sub string) bool { return count(lines, sub) > 0 }

func count(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestEventsOfOtherReplicasDoNotReachTheWatcherTwice(t *testing.T) {
	// Arrange: a local-only subscriber (the watcher's) on replica B, and a normal one.
	bus := NewMemBus()
	h := newHub()
	other := newHub()
	h.attach(context.Background(), bus)
	other.attach(context.Background(), bus)
	local, doneLocal := h.subscribe(localOnly)
	defer doneLocal()
	all, doneAll := h.subscribe(nil)
	defer doneAll()
	time.Sleep(100 * time.Millisecond)

	// Act: one event from the other replica, one from this one.
	other.publish(FeedEvent{Type: "run.finished", ThreadID: "remote", Status: "failed"})
	h.publish(FeedEvent{Type: "run.finished", ThreadID: "mine", Status: "failed"})

	// Assert
	if ev := <-local.ch; ev.ThreadID != "mine" {
		t.Fatalf("the local-only subscriber got %q", ev.ThreadID)
	}
	if ev := <-all.ch; ev.ThreadID != "remote" && ev.ThreadID != "mine" {
		t.Fatalf("unexpected %q", ev.ThreadID)
	}
	select {
	case ev := <-local.ch:
		t.Fatalf("the local-only subscriber got a second event: %q", ev.ThreadID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestClaimIsGrantedToOneReplicaOnly(t *testing.T) {
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	for name, bus := range map[string]FeedBus{"memory": NewMemBus(), "redis": redisx.NewFeedBus(rdb)} {
		a, b := newHub(), newHub()
		a.bus, b.bus = bus, bus

		// Act
		first, second := a.claim("stuck:run1", time.Hour), b.claim("stuck:run1", time.Hour)

		// Assert
		if !first || second {
			t.Fatalf("%s: claims = %v, %v; want true, false", name, first, second)
		}
	}
	if !newHub().claim("anything", time.Hour) {
		t.Fatal("without a bus a claim is always granted")
	}
}

func TestSeenStateIsPerUserOnTheServer(t *testing.T) {
	// Arrange
	r := newTeamRig(t)
	th := r.json("POST", "/api/threads", "alice", `{"projectId":"`+r.project+`"}`, 201)["id"].(string)
	at := "2026-01-02T03:04:05Z"

	// Act
	r.json("POST", "/api/seen", "bob", `{"threadId":"`+th+`","at":"`+at+`"}`, 204)
	r.json("POST", "/api/seen", "bob", `{"threadId":"`+th+`","at":"2026-01-01T00:00:00Z"}`, 204) // older: ignored

	// Assert
	if got := r.json("GET", "/api/seen", "bob", "", 200)[th]; got != at {
		t.Fatalf("bob's last seen = %v, want %s", got, at)
	}
	if got := r.json("GET", "/api/seen", "alice", "", 200); len(got) != 0 {
		t.Fatalf("alice inherited bob's state: %v", got)
	}
	r.json("POST", "/api/seen", "carol", `{"threadId":"`+th+`"}`, 404) // outsiders cannot even probe it
	r.json("POST", "/api/seen", "bob", `{}`, 400)
}
