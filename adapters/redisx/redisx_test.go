package redisx_test

import (
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"

	"github.com/7ar-chat/harness/adapters/redisx"
)

func TestNewConnectsAStandaloneClientWithItsPasswordAndDatabase(t *testing.T) {
	// A standalone client talks to Host:Port with the password and selects the configured database: values land in that database only.
	// Arrange
	mr := miniredis.RunT(t)
	mr.RequireAuth("s3cret")
	host, port := splitAddr(t, mr.Addr())
	rdb := redisx.New(redisx.Options{Host: host, Port: port, Password: "s3cret", DB: 3, PoolSize: 2})
	t.Cleanup(func() { _ = rdb.Close() })
	wrong := redisx.New(redisx.Options{Host: host, Port: port, Password: "nope"})
	t.Cleanup(func() { _ = wrong.Close() })

	// Act
	setErr := rdb.Set(bg, "k", "v", 0).Err()
	wrongErr := wrong.Set(bg, "k", "v", 0).Err()

	// Assert
	if setErr != nil || wrongErr == nil {
		t.Fatalf("with password: %v, with a wrong one: %v", setErr, wrongErr)
	}
	if got, _ := mr.DB(3).Get("k"); got != "v" {
		t.Fatalf("database 3 holds %q", got)
	}
	if mr.DB(0).Exists("k") {
		t.Fatal("the value leaked into database 0")
	}
}

func TestNewWithSentinelFollowsTheMasterTheSentinelNames(t *testing.T) {
	// With Sentinel selected the client asks the listed sentinels (blank entries ignored) where the master is and talks to it, not to Host:Port.
	// Arrange
	master := miniredis.RunT(t)
	decoy := miniredis.RunT(t)
	masterHost, masterPort := splitAddr(t, master.Addr())
	sentinel := miniredis.RunT(t)
	sentinel.Server().Register("SENTINEL", func(c *server.Peer, _ string, args []string) {
		if len(args) == 2 && strings.EqualFold(args[0], "get-master-addr-by-name") && args[1] == "mymaster" {
			c.WriteLen(2)
			c.WriteBulk(masterHost)
			c.WriteBulk(masterPort)
			return
		}
		c.WriteLen(0)
	})
	decoyHost, decoyPort := splitAddr(t, decoy.Addr())
	rdb := redisx.New(redisx.Options{Sentinel: true, MasterName: "mymaster", SentinelHosts: " ," + sentinel.Addr() + " , ", Host: decoyHost, Port: decoyPort})
	t.Cleanup(func() { _ = rdb.Close() })

	// Act
	err := rdb.Set(bg, "k", "v", 0).Err()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := master.Get("k"); got != "v" {
		t.Fatalf("the master holds %q", got)
	}
	if decoy.Exists("k") {
		t.Fatal("the write went to the standalone address")
	}
}
