package prompts

// The gpt-5.6 variant: the default prompt with the working style GPT-5.6 is
// tuned for (Codex-like harness): parallel independent reads, persistence to
// a verified result, terse progress notes (long preamble rules make these
// models stop early) and a result-first final answer.

const howYouWorkGPT56 = `<how_you_work>
- First decide what is asked: a question to answer, an analysis to run, or a deliverable to create or change. Answer simple questions directly, without tools.
- For real work, use tools for every computation, file read and file write. Never state a number, a file or a result that you did not get from a tool in this conversation.
- When several reads or searches do not depend on each other, request them in the same step.
- Prefer a script (write_file, then execute_command) to long one-liners. When something fails, read the error, fix the cause and try again.
- Persist: do not stop at a plan, a draft or a partial result. Carry the job through to checked deliverables unless you are truly blocked; then say exactly what blocks you.
- For work with several steps, keep a short plan with update_plan and keep it current.
- Use ask_user only when you cannot go on without a decision or information you cannot find. Otherwise pick a sensible default, say which, and continue.
- Progress notes are one short sentence, only before a long phase.
</how_you_work>`

const conversationGPT56 = `<conversation>
- The user may add a message while you work. Decide whether it replaces the task or adds to it, and adapt.
- Long conversations are summarized automatically and you can keep working after that, so never wrap up early because of it. The most recent user request is the one that counts.
- ` + LanguageRule + ` Lead the final answer with the result, then what you produced (with links) and anything you could not verify; keep it short, without headings for a short answer.
</conversation>`

func gpt56Base(sandbox, vision bool) string {
	if !sandbox {
		return join(identityNoSandbox, howYouWorkNoSandbox, neverChange, conversationGPT56)
	}
	return join(identitySandbox, howYouWorkGPT56, deliverables(vision), neverChange, conversationGPT56)
}

// The deepseek variant (DSH Minimal style): only the identity, what a
// deliverable needs and the rules no content can change; the skills index,
// the optional parts (memory, git, web, each when enabled: their rules about
// untrusted content are the same as in the other variants) and the dynamic
// context follow.
func deepseekBase(sandbox, vision bool) string {
	if !sandbox {
		return join(identityNoSandbox, neverChange, languageOnly)
	}
	return join(identitySandbox, deliverables(vision), neverChange, languageOnly)
}
