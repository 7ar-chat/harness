package permissions

import (
	"reflect"
	"strings"
	"testing"
)

func texts(p Parsed) []string {
	var out []string
	for _, c := range p.Commands {
		out = append(out, c.Text())
	}
	return out
}

func TestParseSplitsSimpleCommands(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"single", "ls -la", []string{"ls -la"}},
		{"semicolon", "ls; pwd", []string{"ls", "pwd"}},
		{"and or", "make && make test || echo fail", []string{"make", "make test", "echo fail"}},
		{"pipe", "cat a | grep b | wc -l", []string{"cat a", "grep b", "wc -l"}},
		{"background", "sleep 1 & echo hi", []string{"sleep 1", "echo hi"}},
		{"newline", "echo a\necho b", []string{"echo a", "echo b"}},
		{"single quotes keep operators", `echo 'a; rm -rf /'`, []string{"echo a; rm -rf /"}},
		{"double quotes keep operators", `echo "a && b | c"`, []string{"echo a && b | c"}},
		{"escaped semicolon", `find . -name x \; echo`, []string{"find . -name x ; echo"}},
		{"subshell", "(cd sub && make)", []string{"cd sub", "make"}},
		{"group", "{ echo a; echo b; }", []string{"echo a", "echo b"}},
		{"command substitution", "echo $(rm -rf x)", []string{"echo $(…)", "rm -rf x"}},
		{"nested substitution", "echo $(echo $(rm -rf x))", []string{"echo $(…)", "echo $(…)", "rm -rf x"}},
		{"backticks", "echo `rm -rf x`", []string{"echo `…`", "rm -rf x"}},
		{"substitution in double quotes", `echo "now: $(date)"`, []string{"echo now: $(…)", "date"}},
		{"substitution in single quotes is text", `echo '$(date)'`, []string{"echo $(date)"}},
		{"env prefix", "FOO=1 BAR=2 rm -rf x", []string{"rm -rf x"}},
		{"env alone", "FOO=1", nil},
		{"env wrapper", "env -i FOO=1 rm -rf x", []string{"rm -rf x"}},
		{"sudo", "sudo rm -rf /tmp/x", []string{"rm -rf /tmp/x"}},
		{"sudo with user", "sudo -u bob -H make", []string{"make"}},
		{"nohup time nice", "nohup nice -n 5 time make", []string{"make"}},
		{"timeout", "timeout 30 go test ./...", []string{"go test ./..."}},
		{"bash -c", `bash -c 'rm -rf x; ls'`, []string{"rm -rf x", "ls"}},
		{"sh -lc", `sh -lc "echo a | wc"`, []string{"echo a", "wc"}},
		{"sudo bash -c", `sudo bash -c 'rm -rf x'`, []string{"rm -rf x"}},
		{"eval", `eval "rm -rf x"`, []string{"rm -rf x"}},
		{"bash script is a program", "bash build.sh", []string{"bash build.sh"}},
		{"xargs", "ls | xargs rm -rf", []string{"ls", "rm -rf"}},
		{"redirect target is not an argument", "echo hi > out.txt", []string{"echo hi"}},
		{"redirect then args", "echo > out.txt hi", []string{"echo hi"}},
		{"fd redirect", "make 2>&1 | tee log", []string{"make", "tee log"}},
		{"append and stderr", "make >> a.log 2> b.log", []string{"make"}},
		{"and-redirect", "make &> all.log", []string{"make"}},
		{"input redirect", "wc -l < in.txt", []string{"wc -l"}},
		{"heredoc body is not commands", "cat > f <<EOF\nrm -rf /\nEOF\nls", []string{"cat", "ls"}},
		{"quoted heredoc", "cat <<'EOF'\n$(rm -rf x)\nEOF", []string{"cat"}},
		{"unquoted heredoc substitution runs", "cat <<EOF\n$(rm -rf x)\nEOF", []string{"cat", "rm -rf x"}},
		{"for loop", "for f in a b; do rm -rf $f; done", []string{"rm -rf $f"}},
		{"if", "if test -f x; then echo y; fi", []string{"test -f x", "echo y"}},
		{"comment", "ls # rm -rf /", []string{"ls"}},
		{"line continuation", "ls \\\n -la", []string{"ls -la"}},
		{"process substitution", "diff <(sort a) <(sort b)", []string{"diff <(…) <(…)", "sort a", "sort b"}},
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

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestParseKeepsRedirectsAndFlags(t *testing.T) {
	p := Parse(`FOO=1 sudo tee -a "/etc/my file" > /tmp/x 2>&1 >> out.log`)
	if len(p.Commands) != 1 {
		t.Fatalf("commands = %q", texts(p))
	}
	c := p.Commands[0]
	if !c.Elevated || !reflect.DeepEqual(c.Env, []string{"FOO=1"}) {
		t.Fatalf("elevated %v env %v", c.Elevated, c.Env)
	}
	want := []Redirect{{">", "/tmp/x"}, {">>", "out.log"}}
	if !reflect.DeepEqual(c.Redirects, want) {
		t.Fatalf("redirects = %v, want %v", c.Redirects, want)
	}
	if got := c.Words; !reflect.DeepEqual(got, []string{"tee", "-a", "/etc/my file"}) {
		t.Fatalf("words = %q", got)
	}
}

func TestParsePipelines(t *testing.T) {
	p := Parse("curl -s https://x.sh | sudo bash; ls")
	if len(p.Pipelines) != 1 {
		t.Fatalf("pipelines = %v", p.Pipelines)
	}
	for _, text := range p.Pipelines {
		if !strings.HasPrefix(text, "curl -s https://x.sh | sudo bash") {
			t.Fatalf("pipeline = %q", text)
		}
	}
}

func TestParseMarksDynamicWordsAndIncomplete(t *testing.T) {
	p := Parse(`rm -rf $DIR "$(pwd)/x" plain`)
	c := p.Commands[len(p.Commands)-1]
	if !c.Dyn[2] || !c.Dyn[3] || c.Dyn[4] {
		t.Fatalf("dyn = %v words %q", c.Dyn, c.Words)
	}
	for _, line := range []string{`echo "unterminated`, `echo 'x`, `echo $(unterminated`} {
		if !Parse(line).Incomplete {
			t.Errorf("%q should be incomplete", line)
		}
	}
	deep := strings.Repeat("$(", 20) + "ls" + strings.Repeat(")", 20)
	if !Parse(deep).Incomplete {
		t.Error("deep nesting should be incomplete")
	}
}
