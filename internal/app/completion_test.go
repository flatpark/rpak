package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCompletionScriptsParse(t *testing.T) {
	for shell, script := range completionScripts {
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("%s is not installed, not checking its script", shell)
			continue
		}
		file := filepath.Join(t.TempDir(), "rpak."+shell)
		if err := os.WriteFile(file, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(path, "-n", file).CombinedOutput(); err != nil || len(out) != 0 {
			t.Errorf("%s -n: %v\n%s", shell, err, out)
		}
	}
}
