package main

import "testing"

func TestReadOnlyCommands(t *testing.T) {
	cases := map[string]bool{
		"sleep 9; tail -4 /tmp/bench-base.log; echo ---; tail -4 /tmp/x.log": true,
		"cd /work && rg -n 'foo|bar' src | head -20":                         true,
		"grep -c x f 2>/dev/null; ls -la >/dev/null 2>&1":                    true,
		"git -C ../other log --oneline -5 && git status --short":             false,
		"FOO=1 wc -l < input.txt":                                            false,
		"echo \"a > b\" | cut -d' ' -f1":                                     true,
		"pgrep -fl preview &> /dev/null":                                     true,
		"fd -e rs world_rigs src | head; rg -l --type rust NotShadowCaster":  false,
		"fd -H -t f '.log$' /tmp":                                            false,
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
		"wc -l *.go src/*.rs":    true,
		"rg -n 'a{2}' src":       true,
		"ls 2>&1 | head":         true,

		// Ways around the door that were open once: each runs or writes something, or reads a key.
		"cat () ( touch /tmp/pwned ); cat x": false,
		"./cat x":                            false,
		"/tmp/evil/cat x":                    false,
		"LESSOPEN='|touch /tmp/pwned %s' less README.md": false,
		"GIT_EXTERNAL_DIFF=/tmp/evil.sh git diff":        false,
		"LD_PRELOAD=/tmp/evil.so cat x":                  false,
		"echo hi >&/tmp/pwned":                           false,
		"sort -o/tmp/pwned README.md":                    false,
		"sort -uo /tmp/pwned README.md":                  false,
		"sort --compress-program=/tmp/evil.sh README.md": false,
		"uniq README.md /tmp/pwned":                      false,
		"tree -o /tmp/pwned":                             false,
		"less -o /tmp/pwned README.md":                   false,
		"fd -Hx touch /tmp/pwned":                        false,
		"git status":                                     false,
		"cat ~/.ssh/id_ed25519":                          false,
		"cd ~/.ssh && cat id_ed25519":                    false,
		"cat ~/.s*/id*":                                  false,
		"cat ~/.local/state/opencode/service.json":       false,
		"cat /proc/self/environ":                         false,
		"echo $HOME":                                     false,
		"echo \"$HOME\"":                                 false,
		"sed -n 1p *":                                    false,
		"rg x *":                                         false,
		"file -bC x":                                     false,
		"{ cat x; }":                                     false,
		`cat $'\x2essh'/id`:                              false,
		`cat .s\sh/id`:                                   false,
		`cat ".s"sh/id`:                                  false,
		"rg x {--pre=./run,y}":                           false,
		"cat x {fd}>/dev/null":                           false,
		"cat < x":                                        false,
		"[[ -f x ]] && cat x":                            false,
		"for f in a; do cat $f; done":                    false,
		"! grep -q x f && echo none":                     true,
		"grep -rn 'TODO' src 3>/dev/null | wc -l":        true,
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
