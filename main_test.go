package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scotteratigan/kit/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestBashCompletion(t *testing.T) {
	output := bashCompletion("tasks.yaml")

	// Should contain the completion function
	assert.Contains(t, output, "_kit_completions()")
	assert.Contains(t, output, "complete -F _kit_completions kit")

	// Should use correct regex pattern for task names
	assert.Contains(t, output, `grep -E '^  [a-zA-Z0-9_-]+:\s*$'`)

	// Should offer the -with flag
	assert.Contains(t, output, "-with")
}

func TestZshCompletion(t *testing.T) {
	output := zshCompletion("tasks.yaml")

	// Should contain the compdef header
	assert.Contains(t, output, "#compdef kit")

	// Should define the _kit function
	assert.Contains(t, output, "_kit()")

	// Should use correct regex pattern for task names
	assert.Contains(t, output, `grep -E '^  [a-zA-Z0-9_-]+:\s*$'`)

	// Should have guard against running during source
	assert.Contains(t, output, `if [ "$funcstack[1]" = "_kit" ]`)

	// Should conditionally register compdef
	assert.Contains(t, output, "compdef _kit kit")

	// Should offer the -with flag
	assert.Contains(t, output, "'-with[tasks to add as prerequisites]:tasks:'")
}

func TestFishCompletion(t *testing.T) {
	output := fishCompletion("tasks.yaml")

	// Should define the tasks function
	assert.Contains(t, output, "__fish_kit_tasks")

	// Should use correct regex pattern for task names
	assert.Contains(t, output, `grep -E '^  [a-zA-Z0-9_-]+:\s*$'`)

	// Should have completions for flags
	assert.Contains(t, output, "complete -c kit")

	// Should offer the -with flag
	assert.Contains(t, output, "complete -c kit -o with")
}

func TestPrintCompletionInvalidShell(t *testing.T) {
	err := printCompletion("invalid", "tasks.yaml")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported shell: invalid")
}

func TestPrintCompletionValidShells(t *testing.T) {
	// These should not error (output goes to stdout)
	// We can't easily capture stdout, so just verify no error
	for _, shell := range []string{"bash", "zsh", "fish"} {
		// Redirect stdout temporarily
		old := os.Stdout
		_, w, _ := os.Pipe()
		os.Stdout = w

		err := printCompletion(shell, "tasks.yaml")

		w.Close()
		os.Stdout = old

		assert.NoError(t, err, "shell: %s", shell)
	}
}

func TestTaskNameRegexPattern(t *testing.T) {
	// Test the regex pattern we use in completion scripts
	// This simulates what the grep command does

	testYaml := `env:
  AWS_ENDPOINT_URL: http://localhost:4566

tasks:
  clean:
    sh: |
      echo "cleaning"
  build-app:
    command: go build .
    watch: src
  my_task_123:
    sh: echo "test"
`

	// Extract task names using the same pattern as our completion
	lines := strings.Split(testYaml, "\n")
	var tasks []string
	for _, line := range lines {
		// Match: starts with exactly 2 spaces, alphanumeric/hyphen/underscore, colon, end of line
		if len(line) >= 3 && line[0] == ' ' && line[1] == ' ' && line[2] != ' ' {
			// Check if line ends with : (possibly with trailing whitespace)
			trimmed := strings.TrimRight(line, " \t")
			if strings.HasSuffix(trimmed, ":") && !strings.Contains(trimmed, ": ") {
				name := strings.TrimSpace(strings.TrimSuffix(trimmed, ":"))
				tasks = append(tasks, name)
			}
		}
	}

	assert.Equal(t, []string{"clean", "build-app", "my_task_123"}, tasks)
	// Should NOT include AWS_ENDPOINT_URL (has value after colon)
	// Should NOT include sh, command, watch (4-space indent)
}

func TestRunStartupPrintsDefaultConfig(t *testing.T) {
	tempDir := testTempDir(t)
	writeTestConfig(t, filepath.Join(tempDir, "tasks.yaml"), `tasks:
  job:
    command: ["true"]
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "job"}, stdout, stderr)

	assert.Equal(t, 0, exitCode)
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "kit: startup: workflow engine for software development")
	assert.Contains(t, stdout.String(), "version=")
	assert.Contains(t, stdout.String(), "kit: config: path="+filepath.Join(tempDir, "tasks.yaml")+" source=default")
}

func TestRunStartupPrintsExplicitConfig(t *testing.T) {
	tempDir := testTempDir(t)
	configPath := filepath.Join(tempDir, "custom.yaml")
	writeTestConfig(t, configPath, `tasks:
  job:
    command: ["true"]
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "-f", "custom.yaml", "job"}, stdout, stderr)

	assert.Equal(t, 0, exitCode)
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "kit: config: path="+configPath+" source=explicit")
}

func TestRunFlagErrorIsActionable(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-unknown"}, stdout, stderr)

	assert.Equal(t, 1, exitCode)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "kit: error: CLI argument parsing failed")
	assert.Contains(t, stderr.String(), "kit: cause: an unsupported flag or invalid flag value was provided")
	assert.Contains(t, stderr.String(), "kit: next: run `kit -h` to see the supported flags and usage")
}

func TestRunConfigParseErrorIsActionable(t *testing.T) {
	tempDir := testTempDir(t)
	configPath := filepath.Join(tempDir, "tasks.yaml")
	writeTestConfig(t, configPath, "tasks:\n  job: [\n")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir}, stdout, stderr)

	assert.Equal(t, 1, exitCode)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "kit: error: config parse failed for "+configPath+" (source=default)")
	assert.Contains(t, stderr.String(), "kit: cause: the config file contains invalid YAML or unsupported fields")
	assert.Contains(t, stderr.String(), "kit: next: fix the config file and retry")
}

func TestRunWorkflowFailureIsActionable(t *testing.T) {
	tempDir := testTempDir(t)
	writeTestConfig(t, filepath.Join(tempDir, "tasks.yaml"), `tasks:
  job:
    command: ["false"]
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "job"}, stdout, stderr)

	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stdout.String(), "kit: startup: workflow engine for software development")
	assert.Contains(t, stdout.String(), "kit: config: path="+filepath.Join(tempDir, "tasks.yaml")+" source=default")
	assert.Contains(t, stderr.String(), "kit: error: workflow run failed: failed tasks: [job]")
	assert.Contains(t, stderr.String(), "kit: cause: one or more tasks exited with a non-zero status")
	assert.Contains(t, stderr.String(), "kit: next: inspect the task output above or the logs/ directory and retry")
}

func TestInjectPrerequisitesDedupesAndSkipsSelf(t *testing.T) {
	wf := &types.Workflow{Tasks: types.Tasks{
		"prereq": types.Task{},
		"other":  types.Task{},
		"target": types.Task{Dependencies: []string{"prereq"}},
	}}

	err := injectPrerequisites(wf, []string{"target"}, "prereq,other,target")

	assert.NoError(t, err)
	assert.Equal(t, types.Strings{"prereq", "other"}, wf.Tasks["target"].Dependencies)
}

func TestInjectPrerequisitesEmptyIsNoop(t *testing.T) {
	wf := &types.Workflow{Tasks: types.Tasks{"target": types.Task{}}}

	err := injectPrerequisites(wf, []string{"target"}, "")

	assert.NoError(t, err)
	assert.Empty(t, wf.Tasks["target"].Dependencies)
}

func TestRunWithRunsPrerequisiteFirst(t *testing.T) {
	tempDir := testTempDir(t)
	writeTestConfig(t, filepath.Join(tempDir, "tasks.yaml"), `tasks:
  prereq:
    sh: touch marker.txt
  target:
    sh: test -f marker.txt
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "-with", "prereq", "target"}, stdout, stderr)

	assert.Equal(t, 0, exitCode)
	assert.Empty(t, stderr.String())
}

func TestRunWithUnknownTaskIsActionable(t *testing.T) {
	tempDir := testTempDir(t)
	writeTestConfig(t, filepath.Join(tempDir, "tasks.yaml"), `tasks:
  target:
    command: ["true"]
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "-with", "missing", "target"}, stdout, stderr)

	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr.String(), `kit: error: task prerequisite selection failed: prerequisite task "missing" not found in workflow`)
	assert.Contains(t, stderr.String(), "kit: cause: a task listed in `-with` is not defined in the loaded workflow")
	assert.Contains(t, stderr.String(), "kit: next: check the task names in "+filepath.Join(tempDir, "tasks.yaml")+" and retry")
}

func TestRunWithSkipWins(t *testing.T) {
	tempDir := testTempDir(t)
	writeTestConfig(t, filepath.Join(tempDir, "tasks.yaml"), `tasks:
  prereq:
    sh: touch marker.txt
  target:
    sh: test ! -f marker.txt
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "-with", "prereq", "-s", "prereq", "target"}, stdout, stderr)

	assert.Equal(t, 0, exitCode)
	assert.Empty(t, stderr.String())
}

func TestRunWithCycleIsDetected(t *testing.T) {
	tempDir := testTempDir(t)
	writeTestConfig(t, filepath.Join(tempDir, "tasks.yaml"), `tasks:
  parent:
    command: ["true"]
  child:
    command: ["true"]
    dependencies: [parent]
`)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	exitCode := run([]string{"-C", tempDir, "-p", "0", "-with", "child", "parent"}, stdout, stderr)

	assert.Equal(t, 1, exitCode)
	assert.Contains(t, stderr.String(), "dependency cycle detected")
}

func writeTestConfig(t *testing.T, path, contents string) {
	t.Helper()
	err := os.WriteFile(path, []byte(contents), 0644)
	assert.NoError(t, err)
}

// testTempDir returns t.TempDir() with symlinks resolved. On macOS the temp
// dir lives under /var, a symlink to /private/var, while kit prints paths
// based on the resolved working directory.
func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	assert.NoError(t, err)
	return dir
}
