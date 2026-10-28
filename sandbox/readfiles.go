package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// ReadFilesMarker is the first line of the read-files script (sandboxtest fakes it).
const ReadFilesMarker = "# work:read-files"

// readFilesScript prints, for each WORK_PATH_<i> (i < WORK_N) that is a
// regular file reached without a symlink (symlinks, FIFOs, devices,
// directories and missing paths print nothing), "<i> <size> <mtime> U -" when
// its stamp matches WORK_KNOWN_<i> ("size mtime", or "size -" to match the
// size only), else "<i> <size> <mtime> F <base64 of its first bytes>", the
// contents read sharing WORK_BUDGET bytes.
const readFilesScript = ReadFilesMarker + `
set -u
B="$WORK_BUDGET"; i=0
while [ "$i" -lt "$WORK_N" ]; do
  eval "p=\${WORK_PATH_$i}; k=\${WORK_KNOWN_$i:-}"
  if [ -f "$p" ] && [ ! -L "$p" ] && [ "$(realpath -e -- "$p" 2>/dev/null)" = "$p" ]; then
    st=$(stat -c '%s %Y' -- "$p" 2>/dev/null) || st=""
    if [ -n "$st" ]; then
      s=${st%% *}
      if [ -n "$k" ] && { [ "$k" = "$st" ] || [ "$k" = "$s -" ]; }; then
        echo "$i $st U -"
      elif [ "$B" -gt 0 ]; then
        echo "$i $st F $(head -c "$B" -- "$p" | base64 -w0)"
        if [ "$s" -lt "$B" ]; then B=$((B - s)); else B=0; fi
      fi
    fi
  fi
  i=$((i+1))
done
exit 0
`

// FileRead is one file found by ReadRegularFiles.
type FileRead struct {
	Path string
	// Stamp is "size mtime" (seconds).
	Stamp string
	// Unchanged: the stamp matched the known one; Data is not read.
	Unchanged bool
	Data      []byte
}

// ReadRegularFiles reads, with one command, the files among paths (absolute,
// clean) that are regular files reached without a symlink: symlinks, FIFOs,
// devices, directories and missing paths are skipped. known[i] is the stamp
// the caller holds for paths[i] ("size mtime", "size -" for the size only, ""
// none): a file still matching it is returned Unchanged, without its content.
// The contents read share budget bytes (a file read past it is cut).
func ReadRegularFiles(ctx context.Context, sb Sandbox, paths, known []string, budget int64) ([]FileRead, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	env := map[string]string{"WORK_N": strconv.Itoa(len(paths)), "WORK_BUDGET": strconv.FormatInt(budget, 10)}
	for i, p := range paths {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "\x00\n") {
			return nil, fmt.Errorf("read files: %q is not a clean absolute path", p)
		}
		env["WORK_PATH_"+strconv.Itoa(i)] = p
		if i < len(known) {
			env["WORK_KNOWN_"+strconv.Itoa(i)] = known[i]
		}
	}
	var out strings.Builder
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := sb.Exec(cctx, ExecRequest{Command: readFilesScript, Cwd: "/", Timeout: 15 * time.Second, Env: env,
		OnStdout: func(t string) {
			if int64(out.Len()) < budget*2+64*int64(len(paths))+1024 {
				out.WriteString(t)
			}
		}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read files: exit %d", res.ExitCode)
	}
	var files []FileRead
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		i, err := strconv.Atoi(f[0])
		if err != nil || i < 0 || i >= len(paths) {
			continue
		}
		fr := FileRead{Path: paths[i], Stamp: f[1] + " " + f[2], Unchanged: f[3] == "U"}
		if !fr.Unchanged && len(f) > 4 && f[4] != "-" {
			if fr.Data, err = base64.StdEncoding.DecodeString(f[4]); err != nil {
				continue
			}
		}
		files = append(files, fr)
	}
	return files, nil
}
