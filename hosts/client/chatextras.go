package client

import "time"

// The wire format of the chat workbench routes of hosts/httpapi: the queue of messages sent while a reply runs,
// the runtime environments, and the artifacts published into answers.

// QueueItem is a message waiting for the running reply to end (GET /threads/{id}/queue).
type QueueItem struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// Attachments are the files that travel with the message (already stored in the conversation's workspace).
	Attachments []QueuedAttachment `json:"attachments,omitempty"`
}

// QueuedAttachment is a file of a queued message.
type QueuedAttachment struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
}

// QueueAck answers POST /threads/{id}/queue (201): the queued message, or, when the reply had already ended, the
// run that sends it (Run set, Item no longer queued).
type QueueAck struct {
	Item QueueItem `json:"item"`
	Run  *Run      `json:"run,omitempty"`
}

// QueueSendResult answers POST /threads/{id}/queue/{qid}/send: Steered when the message went into the running
// reply, else Run is the run that sends it.
type QueueSendResult struct {
	Steered bool `json:"steered"`
	Run     *Run `json:"run,omitempty"`
}

// Environment is a runtime environment a conversation can run in (GET /environments).
type Environment struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	// Egress summarises the network policy of the environment's sandboxes.
	Egress  string `json:"egress"`
	Default bool   `json:"default,omitempty"`
}

// EnvironmentList is the body of GET /environments.
type EnvironmentList struct {
	Environments []Environment `json:"environments"`
	// Default is the environment of a conversation without a choice.
	Default string `json:"default"`
	// Selectable says whether the UI should offer the choice: several environments, or the server was told to.
	Selectable bool `json:"selectable"`
}

// ThreadEnvironment is the runtime environment of a conversation (GET and PUT /threads/{id}/environment).
type ThreadEnvironment struct {
	// Environment is the name chosen for the thread ("" when none: the default applies).
	Environment string `json:"environment"`
	// Frozen is true once the conversation has a workspace: the environment can no longer change.
	Frozen bool `json:"frozen"`
}

// PublishedArtifact is a workspace file the agent published into an answer (GET /threads/{id}/published).
type PublishedArtifact struct {
	ArtifactID  string    `json:"artifactId"`
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Title       string    `json:"title,omitempty"`
	Version     int       `json:"version"`
	MimeType    string    `json:"mimeType"`
	SizeBytes   int64     `json:"sizeBytes"`
	RunID       string    `json:"runId,omitempty"`
	ToolCallID  string    `json:"toolCallId,omitempty"`
	PublishedAt time.Time `json:"publishedAt"`
}
