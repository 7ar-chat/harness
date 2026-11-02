//go:build unix

package harness_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolNames(o fixedOverhead) string {
	var n []string
	for _, i := range o.Tools {
		n = append(n, i.Name)
	}
	return strings.Join(n, ",")
}

func TestCodingProfileIsLeanerThanWorkByAtLeast40Percent(t *testing.T) {
	// Arrange / Act
	work := measure(t, map[string]string{"AGENT_PROFILE": "work"})
	coding := measure(t, map[string]string{"AGENT_PROFILE": "coding"})

	// Assert
	work.report(t, "profile=work")
	coding.report(t, "profile=coding")
	// codingBefore is the measured coding overhead before the lean profile (21 tools, skills and memory prompts).
	const codingBefore = 5290
	after := coding.TotalTools + coding.TotalSystem
	t.Logf("fixed overhead: work %d tokens, coding before %d, coding now %d (%.1f%% fewer)",
		work.TotalTools+work.TotalSystem, codingBefore, after, 100*float64(codingBefore-after)/codingBefore)
	if after*100 > codingBefore*60 {
		t.Fatalf("coding overhead %d is not at least 40%% below the previous %d", after, codingBefore)
	}
	want := "execute_command,job_output,job_kill,read_file,search_files,write_file,edit_file,list_files,explore,update_plan,ask_user"
	if got := toolNames(coding); got != want {
		t.Fatalf("coding tools = %s\nwant %s", got, want)
	}
	if len(work.Tools) != 20 {
		t.Fatalf("the work profile has %d tools, want 20: %s", len(work.Tools), toolNames(work))
	}
}

func TestCodingToolsAndPromptAreByteStableAcrossBuilds(t *testing.T) {
	// Arrange / Act
	a := measure(t, map[string]string{"AGENT_PROFILE": "coding"})
	b := measure(t, map[string]string{"AGENT_PROFILE": "coding"})

	// Assert
	ja, _ := json.Marshal(a.Tools)
	jb, _ := json.Marshal(b.Tools)
	if string(ja) != string(jb) {
		t.Fatal("two builds of the coding tool list differ")
	}
	if a.System != b.System {
		t.Fatal("two builds of the coding system prompt differ")
	}
}

func TestAgentToolsOverridesTheProfileDefault(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		env     map[string]string
		has     []string
		hasNot  []string
	}{
		{"coding adds memory", "coding", map[string]string{"AGENT_TOOLS": "+memory"}, []string{"memory_search", "memory_capabilities"}, []string{"load_skill"}},
		{"coding adds skills", "coding", map[string]string{"AGENT_TOOLS": "+skills"}, []string{"load_skill"}, []string{"memory_search"}},
		{"SKILLS_IN_CODING", "coding", map[string]string{"SKILLS_IN_CODING": "true"}, []string{"load_skill"}, []string{"memory_get"}},
		{"MEMORY_IN_CODING", "coding", map[string]string{"MEMORY_IN_CODING": "true"}, []string{"memory_get"}, []string{"load_skill"}},
		{"coding removes tools", "coding", map[string]string{"AGENT_TOOLS": "-explore,-ask_user"}, []string{"update_plan"}, []string{"explore", "ask_user"}},
		{"view_file is opt-in", "coding", map[string]string{"AGENT_TOOLS": "+memory"}, nil, []string{"view_file"}},
		{"view_file with a vision model", "coding", map[string]string{"AGENT_TOOLS": "+view_file", "LLM_VISION_MODELS": "gpt-test"}, []string{"view_file"}, nil},
		{"bare list is exact", "coding", map[string]string{"AGENT_TOOLS": "read_file,edit_file"}, []string{"read_file", "edit_file"}, []string{"execute_command", "update_plan"}},
		{"work minus memory", "work", map[string]string{"AGENT_TOOLS": "-memory"}, []string{"view_file", "load_skill"}, []string{"memory_search"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			c.env["AGENT_PROFILE"] = c.profile

			// Act
			o := measure(t, c.env)

			// Assert
			names := "," + toolNames(o) + ","
			for _, n := range c.has {
				if !strings.Contains(names, ","+n+",") {
					t.Errorf("missing %s in %s", n, names)
				}
			}
			for _, n := range c.hasNot {
				if strings.Contains(names, ","+n+",") {
					t.Errorf("unexpected %s in %s", n, names)
				}
			}
		})
	}
}

func TestCodingPromptKeepsMemoryAndSkillsSectionsOnlyWhenEnabled(t *testing.T) {
	// Arrange / Act
	lean := measure(t, map[string]string{"AGENT_PROFILE": "coding"})
	memory := measure(t, map[string]string{"AGENT_PROFILE": "coding", "AGENT_TOOLS": "+memory"})

	// Assert
	if lean.SectionTok["memory"] != 0 {
		t.Fatalf("lean coding prompt has a memory section (%d tokens)", lean.SectionTok["memory"])
	}
	if lean.SectionTok["skills"] != 0 {
		t.Fatalf("lean coding prompt has a skills index (%d tokens)", lean.SectionTok["skills"])
	}
	if memory.SectionTok["memory"] == 0 {
		t.Fatal("+memory did not bring the memory policy back")
	}
}
