package sandboxtools

import "context"

// Coding variants of the tool descriptions (Options.Coding, AGENT_PROFILE=coding):
// the same tools and behaviour, described without the document features
// (PDF/DOCX/XLSX extraction, LibreOffice, deliverables) a coding agent does not
// use. Every request carries these texts, so they are kept short.

const execDescCoding = `Run a bash command (cwd /workspace). Returns JSON {stdout, stderr, exitCode, truncated?, outputPath?}; a non-zero exitCode is reported, not raised.
Long output is cut (head and tail kept); when truncated is true, outputPath (and stderrPath) hold the full output: page it with read_file or search_files.
A command still running after timeoutSeconds (default 120, max 1800) keeps running as a background job and the result is {jobId, status: "running", stdoutTail, stderrTail}; background: true returns that at once. Then use job_output and job_kill.
Use search_files rather than grep, read_file rather than cat. Never delete /workspace/.work/jobs/ (the commands' output files).`

const readDescCoding = `Read a text file (path absolute or relative to /workspace): numbered lines like "cat -n". offset is the 1-based first line (default 1), limit the number of lines (default 2000); a page also stops at about 18,000 characters and its last line gives the offset to continue with.
Images (PNG, JPEG, WEBP, GIF) are shown to models that can see images. Other binary files are refused: inspect them with execute_command.`

const searchDescCoding = `Search file contents with a regular expression (ripgrep): matching lines with their line numbers. path is a directory or a file (default /workspace); glob filters file names (e.g. "*.go"); ignoreCase makes it case-insensitive. .git is not searched.
Returns JSON {matches: [{path, line, text}], truncated} with at most maxResults matches (default 100, max 500). Then read_file with offset to see the context of a match.`

const writeDescCoding = `Create or overwrite a file with the full content given (parent directories are created). Returns JSON {path, bytesWritten}.
For a large file, write it in parts (several files, or write then edit_file). To change an existing file use edit_file.`

const listDescCoding = `List files and directories. path defaults to /workspace, depth to 2 (at most 5). Returns JSON {path, entries: [{path, type, size}], truncated?} with at most 500 entries.`

const editDescCoding = `Replace text in an existing file. oldText must match exactly once (or set replaceAll); if there is no exact match, a unique match that ignores indentation and trailing spaces is used. Returns JSON {path, replacements, matched}.
Read the file with read_file first and copy oldText from it: an edit is refused when you have not read the file in this conversation or it changed since you last saw it (re-read it). On a miss the error shows the closest lines.`

// readInputCoding is read_file's input without the document-only fields.
type readInputCoding struct {
	Path   string `json:"path" jsonschema_description:"File path, absolute or relative to /workspace"`
	Offset int    `json:"offset,omitempty" jsonschema_description:"1-based line to start from (default 1)"`
	Limit  int    `json:"limit,omitempty" jsonschema_description:"Maximum number of lines (default 2000)"`
}

func (b *binding) readCoding(ctx context.Context, in readInputCoding) (string, error) {
	return b.read(ctx, readInput{Path: in.Path, Offset: in.Offset, Limit: in.Limit})
}

// descriptions are the tool texts of a profile.
type descriptions struct{ exec, read, search, write, list, edit string }

var (
	workDescriptions   = descriptions{execDesc, readDesc, searchDesc, writeDesc, listDesc, editDesc}
	codingDescriptions = descriptions{execDescCoding, readDescCoding, searchDescCoding, writeDescCoding, listDescCoding, editDescCoding}
)
