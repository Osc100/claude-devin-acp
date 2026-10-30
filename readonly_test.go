package main

import "testing"

func TestReadOnlyCommands(t *testing.T) {
	cases := map[string]bool{
		"sleep 9; tail -4 /tmp/bench-base.log; echo ---; tail -4 /tmp/x.log": true,
		"cd /work && rg -n 'foo|bar' src | head -20":                         true,
		"grep -c x f 2>/dev/null; ls -la >/dev/null 2>&1":                    true,
		"git -C ../other log --oneline -5 && git status --short":             true,
		"FOO=1 wc -l < input.txt":                                            true,
		"echo \"a > b\" | cut -d' ' -f1":                                     true,
		"pgrep -fl preview &> /dev/null":                                     true,
		"fd -e rs world_rigs src | head; rg -l --type rust NotShadowCaster":  true,
		"fd -H -t f '.log$' /tmp":                                            true,
		"fd -e tmp -x rm":                                                    false,
		"fd -e rs --exec-batch sed -i s/a/b/":                                false,
		"rg --pre=./run x":                                                   false,
		"tail -3 log > out.txt":                                              false,
		"echo hi >> notes":                                                   false,
		"rm -rf build":                                                       false,
		"sleep 9; rm x":                                                      false,
		"cat $(ls)":                                                          false,
		"echo \"$(whoami)\"":                                                 false,
		"cat `ls`":                                                           false,
		"find . -name x -delete":                                             false,
		"find . -exec rm {} ;":                                               false,
		"rg --pre ./run x":                                                   false,
		"sort -o out in":                                                     false,
		"git push origin main":                                               false,
		"git diff --output=patch":                                            false,
		"diff <(ls a) <(ls b)":                                               false,
		"echo 'unterminated":                                                 false,
		"sed -n '560,640p' /tmp/f.rs && sed -n '1578,1582p;1677,1681p' g": true,
		"sed -n '1,$p' f":        true,
		"sed -n 's/a/b/w out' f": false,
		"sed '5p' f":             false,
		"sed -i s/a/b/ f":        false,
		"bash -c 'tail x'":       false,
	}
	for command, want := range cases {
		call := map[string]any{"kind": "execute", "rawInput": map[string]any{"command": command}}
		if got := readOnly(call); got != want {
			t.Errorf("%q: read-only %v, want %v", command, got, want)
		}
	}
	if readOnly(map[string]any{"kind": "edit", "rawInput": map[string]any{"command": "ls"}}) {
		t.Error("an edit is read-only")
	}
}
