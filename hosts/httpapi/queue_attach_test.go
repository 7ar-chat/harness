package httpapi_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// png1x1 is a valid 1x1 PNG, base64.
const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func TestAQueuedMessageCarriesItsAttachmentsAndSendsThem(t *testing.T) {
	// Arrange: a reply that keeps running while a message with an image is queued
	model := newGatedModel()
	r := newRig(t, rigOpts{model: model.Model})
	id := r.newThread("alice")
	r.startRun("alice", id, "first")
	r.waitFor("the first reply to start", func() bool { return len(model.replies()) == 1 })

	// Act
	ack := r.json("POST", "/api/threads/"+id+"/queue", "alice",
		`{"message":"see the screenshot","attachments":[{"name":"shot.png","mime":"image/png","data":"`+png1x1+`"}]}`, 201)

	// Assert: the queued item shows the file
	item := ack["item"].(map[string]any)
	atts, _ := item["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["name"] != "shot.png" || atts[0].(map[string]any)["mimeType"] != "image/png" {
		t.Fatalf("queued item = %v", item)
	}
	if q := r.queue("alice", id); len(q) != 1 || q[0]["attachments"] == nil {
		t.Fatalf("queue = %v", q)
	}

	// Act: the first reply ends and the queued message is sent
	close(model.release)
	r.waitFor("the queue to drain", func() bool { return len(model.replies()) >= 2 && len(r.queue("alice", id)) == 0 })
	r.waitIdle(id, "alice")

	// Assert: the user message that was sent has the attachment
	th := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	var found bool
	for _, m := range th["messages"].([]any) {
		m := m.(map[string]any)
		if m["role"] == "user" && m["content"] == "see the screenshot" {
			raw, _ := json.Marshal(m)
			found = strings.Contains(string(raw), "shot.png")
		}
	}
	if !found {
		t.Fatalf("the sent message lost its attachment: %v", th["messages"])
	}
}

func TestQueuedAttachmentsAreValidatedAndAQueueCanBeDiscarded(t *testing.T) {
	// Arrange
	model := newGatedModel()
	r := newRig(t, rigOpts{model: model.Model})
	id := r.newThread("alice")
	r.startRun("alice", id, "first")
	r.waitFor("the first reply to start", func() bool { return len(model.replies()) == 1 })

	// Act / Assert: a file that is no image is refused, nothing is queued
	r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"x","attachments":[{"name":"a.pdf","mime":"application/pdf","data":"JVBERg=="}]}`, 400)
	big := strings.Repeat("A", 8<<20)
	r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"x","attachments":[{"name":"big.png","mime":"image/png","data":"`+big+`"}]}`, 400)
	if q := r.queue("alice", id); len(q) != 0 {
		t.Fatalf("a refused message was queued: %v", q)
	}

	// Act: discard everything that waits
	r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"one"}`, 201)
	r.json("POST", "/api/threads/"+id+"/queue", "alice", `{"message":"two"}`, 201)
	resp := r.do("DELETE", "/api/threads/"+id+"/queue", "alice", "")
	body, _ := io.ReadAll(resp.Body)

	// Assert
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"discarded":2`) {
		t.Fatalf("discard: %d %s", resp.StatusCode, body)
	}
	if q := r.queue("alice", id); len(q) != 0 {
		t.Fatalf("queue = %v", q)
	}
	if resp := r.do("DELETE", "/api/threads/"+id+"/queue", "mallory", ""); resp.StatusCode != 404 {
		t.Fatalf("another user discarding: %d", resp.StatusCode)
	}
	close(model.release)
	r.waitIdle(id, "alice")
}
