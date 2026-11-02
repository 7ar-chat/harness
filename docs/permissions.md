# Permissions and git safety

`SANDBOX_BACKEND=local` runs commands on your machine with your rights, so a permission layer
(`permissions/`) sits in front of `execute_command`, `write_file` and `edit_file` whenever the local
backend or `AGENT_PROFILE=coding` is on (the work profile on OpenSandbox is unchanged). Each call is
allowed, asked or denied; an *ask* pauses the run with the usual approval interrupt and resumes when
approved, a *deny* never runs and the model gets a clear tool error. Shell lines are split into their
simple commands (`; && || |`, subshells, `$(...)`, backticks, `bash -c`, `sudo`, `FOO=1 cmd`,
redirections) and the most severe verdict wins.

| Default | Examples |
|---|---|
| allow | read-only commands (`ls cat grep find git status/diff/log`), build/test/lint (`go test/build/vet`, `make`, `npm test`, `pytest`, `cargo test`), file work inside the workspace (`mkdir cp mv rm`, `git add/commit`), writes inside the workspace |
| ask | network (`git push/pull/fetch`, `curl`, `wget`, `ssh`), installs (`pip/npm install`, `apt`, `brew`), `sudo`, `docker rm/run`, `chmod -R`, `dd`, `rm -rf` or any write outside the workspace, `git reset --hard/clean/rebase`, unknown programs and scripts |
| deny | `rm -rf /` or `~`, fork bombs, `mkfs`/`dd` on devices, `git push --force` to main/master, writes to `.git/`, `.env`, `id_rsa`, `*.pem` (unless explicitly allowed) |

`AGENT_PERMISSIONS` picks the mode: `ask` (default on local), `auto-edit` (edits free everywhere,
commands asked), `yolo` (no prompts, deny rules still apply), `yolo-unsafe` (nothing is checked),
`readonly` (only read-only commands, no edits). Rules go in `.har/permissions.yml` (workspace) and
`~/.har/permissions.yml`, or in `AGENT_PERMISSIONS_ALLOW|ASK|DENY` (entries separated by
newlines or `;;`); deny beats ask beats allow, and a user allow overrides a built-in ask:
A project's permissions (`.har/permissions.yml` and `permissions.*` of its `config.yml`) can only **tighten**: its `ask` and
`deny` rules and a `mode` as strict as yours (or stricter) are honoured; its `allow` rules and a looser `mode` are
ignored unless the project is trusted (`harness trust`). The ignored items are logged once and listed by `-config-dump`.

```yaml
mode: ask
allow: ["git push origin feature/*", "make deploy-staging", "write_file(.env.local)"]
ask:   ["docker*"]
deny:  ["curl*|sh", "Bash(terraform apply*)", "write_file(migrations/**)"]
```

An entry is `execute_command(glob)` (alias `Bash`, or a bare glob such as `git push*`; `*` matches
anything, a `|` matches a pipeline), `write_file(path-glob)` (alias `Edit`, covers `edit_file`) or
`any_tool(glob on its JSON arguments)`. Globs on commands match text, so `rm -rf*` does not see
`rm -r -f`; the built-in checks do. Paths are lexical: symlinks out of the workspace are not followed.

`gitsafe/` snapshots the work tree at the start of every local run (`git add -A` into a private index
plus a commit object kept under `refs/harness/snapshots/<thread>/<n>`; no commit on your branch, no
index or stash change) and `gitsafe.Restore(ctx, dir, ref)` / `gitsafe.Undo(ctx, dir, thread)` put files,
HEAD and the staged state back, saving the current state under `refs/harness/backups/` first.
Nothing happens outside a git repository.
