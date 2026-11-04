package sandbox

import "context"

// Endpoint is how the harness reaches a TCP port inside a sandbox over HTTP.
type Endpoint struct {
	// URL is the base address (http://host:port), without a path.
	URL string
	// Headers must accompany every request (a sandbox provider's access token).
	Headers map[string]string
}

// PortExposer is a sandbox whose listening ports can be reached from the harness: the preview of a project's dev
// server goes through it. Backends that cannot reach a port do not implement it.
type PortExposer interface {
	Endpoint(ctx context.Context, port int) (Endpoint, error)
}

// PortsOf returns the port access of sb, looking through the wrappers a Manager puts around a sandbox; nil when the
// backend has none.
func PortsOf(sb Sandbox) PortExposer {
	for {
		switch v := sb.(type) {
		case PortExposer:
			return v
		case managed:
			if p, ok := v.Instance.(PortExposer); ok {
				return p
			}
			return nil
		case noticed:
			sb = v.Sandbox
		case recreated:
			sb = v.Sandbox
		default:
			return nil
		}
	}
}
