package store

// UserSandbox is a live thread of a user with a recorded sandbox.
type UserSandbox struct {
	ThreadID  string
	SandboxID string
}
