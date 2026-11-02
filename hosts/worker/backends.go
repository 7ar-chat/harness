package worker

// The worker is a deployable process that chooses its sandbox backend from SANDBOX_BACKEND, so it carries the backends
// that can be named there. (An application embedding the harness imports only the ones it uses.)
import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
