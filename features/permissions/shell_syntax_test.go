package permissions

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseUnderstandsQuotingAndEscapes(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"escaped space joins a word", `touch my\ file`, []string{"touch my file"}},
		{"escaped quote stays text", `echo \"hi\"`, []string{`echo "hi"`}},
		{"escape at the end of the line", `echo a\`, []string{"echo a"}},
		{"quotes concatenate with text", `echo a"b c"'d e'f`, []string{"echo ab cd ef"}},
		{"empty quoted word counts", `grep '' file`, []string{"grep  file"}},
		{"escaped dollar in double quotes", `echo "cost: \$5 and \\ and \" and \x"`, []string{`echo cost: $5 and \ and " and \x`}},
		{"line continuation in double quotes", "echo \"a\\\nb\"", []string{"echo ab"}},
		{"backticks in double quotes", "echo \"on `hostname`\"", []string{"hostname", "echo on `…`"}},
		{"arithmetic is one dynamic word", `echo $((1+2)) done`, []string{"echo $((…)) done"}},
		{"double semicolon", "echo a;; echo b", []string{"echo a", "echo b"}},
		{"stderr pipe", "make |& tee log", []string{"make", "tee log"}},
		{"background and", "a && b & c", []string{"a", "b", "c"}},
		{"or", "a || b", []string{"a", "b"}},
		{"comment only", "# nothing to run", nil},
		{"hash inside a word is text", "echo a#b", []string{"echo a#b"}},
		{"carriage returns are blanks", "echo a\r\necho b", []string{"echo a", "echo b"}},
		{"variable in word", "echo ${HOME}/x", []string{"echo ${HOME}/x"}},
		{"lone dollar", "echo $ and $?", []string{"echo $ and $?"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := texts(Parse(tt.line))
			if !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Fatalf("Parse(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestParseKeepsOnlyOutputRedirections(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []Redirect
	}{
		{"truncate", "echo a > f", []Redirect{{">", "f"}}},
		{"append", "echo a >> f", []Redirect{{">>", "f"}}},
		{"clobber", "echo a >| f", []Redirect{{">", "f"}}},
		{"no space", "echo a >f", []Redirect{{">", "f"}}},
		{"stderr to file", "make 2> err.log", []Redirect{{">", "err.log"}}},
		{"both to file", "make &> all.log", []Redirect{{"&>", "all.log"}}},
		{"both append", "make &>> all.log", []Redirect{{"&>>", "all.log"}}},
		{"quoted target", `echo a > "my file"`, []Redirect{{">", "my file"}}},
		{"descriptor copy is not a file", "make 2>&1", nil},
		{"descriptor copy to stdout", "echo a >&2", nil},
		{"descriptor close", "echo a >&-", nil},
		{"descriptor move", "echo a >&2-", nil},
		{"bash both-to-file form", "echo a >& /etc/hosts", []Redirect{{">", "/etc/hosts"}}},
		{"bash both-to-file form without space", "echo a >&/etc/hosts", []Redirect{{">", "/etc/hosts"}}},
		{"input is not output", "wc -l < in.txt", nil},
		{"here string is not output", "cat <<< hello", nil},
		{"quoted digits are an argument", `echo "2">f`, []Redirect{{">", "f"}}},
		{"several", "make > a 2> b", []Redirect{{">", "a"}, {">", "b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Parse(tt.line)
			if len(p.Commands) != 1 {
				t.Fatalf("Parse(%q) commands = %q", tt.line, texts(p))
			}
			if got := p.Commands[0].Redirects; !reflect.DeepEqual(got, tt.want) && !(len(got) == 0 && len(tt.want) == 0) {
				t.Fatalf("Parse(%q) redirects = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestParseKeepsAQuotedNumberBeforeARedirectAsAnArgument(t *testing.T) {
	// "2">f is echo "2" redirected to f, while 2>f is stderr redirected to f.
	// Arrange / Act
	quoted := Parse(`echo "2">f`).Commands[0]
	bare := Parse(`echo 2>f`).Commands[0]

	// Assert
	if !reflect.DeepEqual(quoted.Words, []string{"echo", "2"}) || !reflect.DeepEqual(bare.Words, []string{"echo"}) {
		t.Fatalf("quoted %q bare %q", quoted.Words, bare.Words)
	}
}

func TestParseRedirectOnlyCommandStillWritesItsFile(t *testing.T) {
	// `> file` alone truncates the file: it must stay visible to the policy.
	// Arrange / Act
	p := Parse("> /etc/hosts")

	// Assert
	if len(p.Commands) != 1 || len(p.Commands[0].Words) != 0 || !reflect.DeepEqual(p.Commands[0].Redirects, []Redirect{{">", "/etc/hosts"}}) {
		t.Fatalf("commands %+v", p.Commands)
	}
	if p.Commands[0].Prog() != "" || p.Commands[0].Text() != "" {
		t.Fatalf("prog %q text %q for a command without words", p.Commands[0].Prog(), p.Commands[0].Text())
	}
}

func TestSimpleProgIsTheBaseNameOfTheProgram(t *testing.T) {
	// Arrange / Act
	c := Parse("/usr/bin/git status").Commands[0]
	relative := Parse("./scripts/run.sh").Commands[0]

	// Assert
	if c.Prog() != "git" || relative.Prog() != "run.sh" {
		t.Fatalf("progs %q %q", c.Prog(), relative.Prog())
	}
}

func TestParseHeredocs(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"dash strips tabs of the delimiter", "cat <<-EOF\n\trm -rf /\n\tEOF\nls", []string{"cat", "ls"}},
		{"double-quoted delimiter is quoted", "cat <<\"EOF\"\n$(rm -rf x)\nEOF", []string{"cat"}},
		{"escaped delimiter is quoted", "cat <<\\EOF\n$(rm -rf x)\nEOF", []string{"cat"}},
		{"delimiter followed by a pipe", "cat <<EOF | wc -l\nrm -rf /\nEOF", []string{"cat", "wc -l"}},
		{"delimiter followed by a command", "cat <<EOF; ls\nbody\nEOF", []string{"cat", "ls"}},
		{"backticks in an unquoted body run", "cat <<EOF\n`rm -rf x`\nEOF", []string{"cat", "rm -rf x"}},
		{"escaped substitution in a body is text", "cat <<EOF\n\\$(rm -rf x) \\`y\\`\nEOF", []string{"cat"}},
		{"unterminated unquoted body still runs its substitutions", "cat <<EOF\n$(rm -rf x)\n", []string{"cat", "rm -rf x"}},
		{"unterminated quoted body hides its text", "cat <<'EOF'\n$(rm -rf x)\n", []string{"cat"}},
		{"two heredocs on one line", "cat <<A <<B\n$(one)\nA\n$(two)\nB\nls", []string{"cat", "one", "two", "ls"}},
		{"heredoc without delimiter", "cat <<", []string{"cat"}},
		{"delimiter in a subshell", "(cat <<EOF\nx\nEOF\n)", []string{"cat"}},
		{"a line that only starts like the delimiter is body", "cat <<EOF\nEOFX\n$(rm -rf y)\nEOF\nls", []string{"cat", "rm -rf y", "ls"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := texts(Parse(tt.line))
			if !sameSet(got, tt.want) {
				t.Fatalf("Parse(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestParseSubstitutionsNest(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
		bad  bool // incomplete
	}{
		{"parens and quotes inside", `echo $(echo ")" ')' x)`, []string{"echo $(…)", "echo ) ) x"}, false},
		{"escaped paren inside", `echo $(echo \) x)`, []string{"echo $(…)", "echo ) x"}, false},
		{"escaped quote inside double quotes", `echo $(echo "a\"b)c")`, []string{"echo $(…)", `echo a"b)c`}, false},
		{"subshell inside", "echo $( (cd x && ls) )", []string{"echo $(…)", "cd x", "ls"}, false},
		{"backtick with an escaped backtick", "echo `echo \\`x\\``", []string{"echo `…`", "echo `x`"}, false},
		{"process substitution to write", "tee >(wc -l) < in", []string{"tee <(…)", "wc -l"}, false},
		{"unterminated process substitution", "diff <(sort a", []string{"diff <(…)", "sort a"}, true},
		{"unterminated backtick", "echo `date", []string{"echo `…`", "date"}, true},
		{"unterminated substitution in double quotes", `echo "$(date`, []string{"echo $(…)", "date"}, true},
		{"empty substitution", "echo $()", []string{"echo $(…)"}, false},
		{"unbalanced inside a quote in the substitution", `echo $(echo "x)`, []string{"echo $(…)", "echo x)"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Parse(tt.line)
			if got := texts(p); !sameSet(got, tt.want) {
				t.Fatalf("Parse(%q) = %q, want %q", tt.line, got, tt.want)
			}
			if p.Incomplete != tt.bad {
				t.Fatalf("Parse(%q) incomplete = %v, want %v", tt.line, p.Incomplete, tt.bad)
			}
		})
	}
}

func TestParseMarksVariablesInsideDoubleQuotesAsDynamic(t *testing.T) {
	// Arrange / Act
	c := Parse(`rm "$DIR/x" '$DIR' "a$" ${HOME} $1 "$(pwd)"`).Commands
	last := c[len(c)-1]

	// Assert
	want := map[int]bool{1: true, 4: true, 5: true, 6: true}
	if !reflect.DeepEqual(last.Dyn, want) {
		t.Fatalf("dyn = %v, want %v (words %q)", last.Dyn, want, last.Words)
	}
}

func TestParseUnwrapsWrappers(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"sudo option with value then program", "sudo -u root -- rm -rf x", []string{"rm -rf x"}},
		{"sudo with trailing value option only", "sudo -u", nil},
		{"sudo env assignment", "sudo -E FOO=1 make", []string{"make"}},
		{"doas", "doas make", []string{"make"}},
		{"env unset option", "env -u FOO -i BAR=1 make", []string{"make"}},
		{"env option missing its value", "env -u", nil},
		{"env alone", "env", nil},
		{"command lookup is not an execution", "command -v rm", []string{"command -v rm"}},
		{"command runs the program", "command -p ls", []string{"ls"}},
		{"builtin and exec", "exec ls", []string{"ls"}},
		{"nice with number", "nice -n 5 make", []string{"make"}},
		{"timeout with signal option", "timeout -k 5 -s KILL 30 make", []string{"make"}},
		{"timeout with only a duration", "timeout 30", nil},
		{"xargs with value options", "ls | xargs -I {} -n 1 rm {}", []string{"ls", "rm {}"}},
		{"bash with options before -c", "bash -e -c 'rm -rf x'", []string{"rm -rf x"}},
		{"bash -c without a script", "bash -c", []string{"bash -c"}},
		{"bash long option is not -c", "bash --noprofile build.sh", []string{"bash --noprofile build.sh"}},
		{"bash script name stops the scan", "bash build.sh -c x", []string{"bash build.sh -c x"}},
		{"nested wrappers", "sudo nohup timeout 5 bash -c 'rm -rf x; ls'", []string{"rm -rf x", "ls"}},
		{"eval joins its words", "eval rm -rf x", []string{"rm -rf x"}},
		{"if/then keywords", "if true; then rm x; else rm y; fi", []string{"true", "rm x", "rm y"}},
		{"while loop", "while read l; do echo $l; done", []string{"read l", "echo $l"}},
		{"negation", "! grep x f", []string{"grep x f"}},
		{"case header holds no command", "case $x in", nil},
		{"assignment then wrapper", "A=1 sudo B=2 env C=3 make", []string{"make"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := texts(Parse(tt.line))
			if !sameSet(got, tt.want) {
				t.Fatalf("Parse(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestParseRecordsEnvAndElevationOfWrappedCommands(t *testing.T) {
	// Arrange / Act
	c := Parse("A=1 sudo B=2 env C=3 make all").Commands
	nested := Parse("sudo bash -c 'make; rm x'").Commands
	xargs := Parse("ls | xargs rm").Commands

	// Assert
	if len(c) != 1 || !reflect.DeepEqual(c[0].Env, []string{"A=1", "B=2", "C=3"}) || !c[0].Elevated || c[0].Raw != "sudo B=2 env C=3 make all" {
		t.Fatalf("wrapped: %+v", c)
	}
	if len(nested) != 2 || !nested[0].Elevated || !nested[1].Elevated {
		t.Fatalf("commands of a sudo bash -c are not all elevated: %+v", nested)
	}
	if len(xargs) != 2 || xargs[0].Dynamic || !xargs[1].Dynamic {
		t.Fatalf("xargs: %+v", xargs)
	}
}

func TestParseKeepsTheRedirectsOfAWrappedScript(t *testing.T) {
	// A redirect on `bash -c '…' > file` writes that file: it is kept as a command of its own so it is judged.
	// Arrange / Act
	cmds := Parse(`bash -c 'echo hi' > /etc/motd`).Commands

	// Assert
	var written []Redirect
	for _, c := range cmds {
		written = append(written, c.Redirects...)
	}
	if !reflect.DeepEqual(written, []Redirect{{">", "/etc/motd"}}) {
		t.Fatalf("commands %+v", cmds)
	}
}

func TestParsePipelineText(t *testing.T) {
	// Arrange / Act
	p := Parse("cat a | sort | uniq; ls | wc")

	// Assert
	if len(p.Pipelines) != 2 {
		t.Fatalf("pipelines %v", p.Pipelines)
	}
	var texts []string
	for _, s := range p.Pipelines {
		texts = append(texts, s)
	}
	if !sameSet(texts, []string{"cat a | sort | uniq", "ls | wc"}) {
		t.Fatalf("pipelines %q", texts)
	}
	ids := map[int]int{}
	for _, c := range p.Commands {
		ids[c.Pipeline]++
	}
	if len(ids) != 2 {
		t.Fatalf("pipeline ids %v", ids)
	}
}

func TestParseRefusesToGuessPastTheNestingLimit(t *testing.T) {
	// Exactly the limit parses; one more level is Incomplete.
	// Arrange
	nest := func(n int) string { return strings.Repeat("echo $(", n) + "ls" + strings.Repeat(")", n) }

	// Act
	atLimit := Parse(nest(maxDepth))
	over := Parse(nest(maxDepth + 1))

	// Assert
	if atLimit.Incomplete || !over.Incomplete {
		t.Fatalf("at the limit incomplete=%v, over it incomplete=%v", atLimit.Incomplete, over.Incomplete)
	}
}

func TestParseHelpers(t *testing.T) {
	// isDigits and isAssign decide what a descriptor and an env prefix are.
	for s, want := range map[string]bool{"": false, "2": true, "10": true, "2a": false, "-1": false, "a": false} {
		if isDigits(s) != want {
			t.Errorf("isDigits(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{"A=1": true, "_a1=": true, "a_b=x=y": true, "=x": false, "1A=x": false, "A-B=x": false, "plain": false, "A.B=x": false} {
		if isAssign(s) != want {
			t.Errorf("isAssign(%q) = %v", s, !want)
		}
	}
	if got := matchParen([]rune("(a (b) c"), 0); got != len([]rune("(a (b) c")) {
		t.Errorf("unbalanced matchParen = %d", got)
	}
	if got := indexRune([]rune(`a\`+"`"+"b`c"), 0, '`'); got != 4 {
		t.Errorf("indexRune skipped escapes wrongly: %d", got)
	}
}
