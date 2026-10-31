package main

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

// Read-only commands are allowed without the reviewer: a shell command each of whose simple
// commands (joined by ; && || | & and newlines) only reads, with nothing that writes or runs
// something else. Devin asks for these when one part is missing from its own allow list, as in
// `sleep 9; tail -4 log`, and they are most of what it asks for while it waits on a build.
//
// It is a narrow door, and anything it is unsure of goes to the reviewer instead. The command is
// parsed as bash parses it (mvdan.cc/sh), and taken only when it is simple commands and nothing
// else: each named bare (not a path, which could be a script of the same name), with no variable
// before it (LESSOPEN=, LD_PRELOAD= and the like run code), no expansion ($, `, $(), <()), no
// definitions, groups or subshells, redirects only to /dev/null or another descriptor, and no word
// naming credentials. Commands that can write or run something with the right flag (sort -o, find
// -exec, git's config hooks) are not here at all.

// readers are the commands taken as only reading, each with the arguments that would make it do
// more, refused.
var readers = map[string][]string{
	"cat": nil, "head": nil, "tail": nil, "wc": nil, "nl": nil, "tac": nil,
	"ls": nil, "stat": nil, "du": nil, "df": nil,
	"file": {"-C", "--compile"},
	"grep": nil, "egrep": nil, "fgrep": nil,
	"rg":  {"--pre", "--pre-glob"},
	"cut": nil, "tr": nil, "diff": nil, "cmp": nil, "column": nil,
	"echo": nil, "printf": nil, "sleep": nil, "true": nil, "false": nil, "test": nil,
	"pwd": nil, "cd": nil, "date": nil, "which": nil, "whoami": nil, "uname": nil, "id": nil,
	"basename": nil, "dirname": nil, "realpath": nil, "readlink": nil,
	"pgrep": nil, "ps": nil, "nproc": nil, "free": nil, "uptime": nil,
	"sed": nil, // only printing lines: see sedPrint
}

// sedPrint is a sed script that only prints lines by number, as `sed -n '560,640p;700p'`.
var sedPrint = regexp.MustCompile(`^(\d+|\$)(,(\d+|\$))?p(;(\d+|\$)(,(\d+|\$))?p)*;?$`)

// secrets are what a word naming credentials holds somewhere in it.
var secrets = []string{
	".ssh", ".gnupg", ".aws", ".kube", ".docker", ".netrc", ".git-credentials", ".env",
	"credentials", "keyring", "id_rsa", "id_ed25519", "id_ecdsa", "auth.json", "service.json",
	"claude-devin-acp", "/environ",
}

// word is one word of a command as the shell leaves it, and whether the shell expands it into file
// names (a glob or a brace): names that can be anything, flags included.
type word struct {
	text    string
	expands bool
}

// readOnly is whether a tool call is a shell command that only reads.
func readOnly(call map[string]any) bool {
	if kind, _ := call["kind"].(string); kind != "execute" {
		return false
	}
	input, _ := call["rawInput"].(map[string]any)
	command, _ := input["command"].(string)
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) == 0 {
		return false
	}
	for _, stmt := range file.Stmts {
		if !readerStmt(stmt) {
			return false
		}
	}
	return true
}

// readerStmt is whether a statement is simple commands that only read, joined by && || or pipes.
func readerStmt(stmt *syntax.Stmt) bool {
	if stmt.Coprocess || stmt.Disown {
		return false
	}
	for _, redirect := range stmt.Redirs {
		if !harmless(redirect) {
			return false
		}
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		return readerStmt(cmd.X) && readerStmt(cmd.Y)
	case *syntax.CallExpr:
		if len(cmd.Assigns) > 0 || len(cmd.Args) == 0 {
			return false
		}
		words := make([]word, len(cmd.Args))
		for i, arg := range cmd.Args {
			w, ok := literal(arg)
			if !ok {
				return false
			}
			words[i] = w
		}
		return readerCall(words)
	}
	return false // a function, a group, a subshell, a loop, [[ ]]...
}

// harmless is whether a redirect only discards output or points it at another descriptor.
func harmless(r *syntax.Redirect) bool {
	if r.N != nil && strings.Trim(r.N.Value, "0123456789") != "" {
		return false // {varname}>
	}
	target, ok := literal(r.Word)
	if !ok || target.expands {
		return false
	}
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll:
		return target.text == "/dev/null"
	case syntax.DplOut:
		return target.text == "-" || strings.Trim(target.text, "0123456789") == "" // >&2, >&-; not >&file
	}
	return false // input, here-documents
}

// literal is a word as the shell leaves it, if it is nothing but literal text and quotes: no
// expansion of any kind, which could make it anything.
func literal(w *syntax.Word) (word, bool) {
	if w == nil {
		return word{}, false
	}
	var b strings.Builder
	expands := false
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if pattern.HasMeta(p.Value, 0) || strings.Contains(p.Value, "{") {
				expands = true
			}
			b.WriteString(unescape(p.Value))
		case *syntax.SglQuoted:
			if p.Dollar {
				return word{}, false // $'...', whose escapes could spell anything
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return word{}, false
			}
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return word{}, false
				}
				b.WriteString(dqUnescape.Replace(lit.Value))
			}
		default:
			return word{}, false
		}
	}
	return word{b.String(), expands}, true
}

// unescape is unquoted text as the shell leaves it: a backslash quotes the character after it.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// dqUnescape is the escapes a double-quoted string takes.
var dqUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\$`, `$`, "\\`", "`", "\\\n", "")

// readerCall is whether one simple command, as words, only reads.
func readerCall(words []word) bool {
	name := words[0].text
	refused, known := readers[name]
	if !known || words[0].expands {
		return false
	}
	for _, w := range words[1:] {
		if namesSecret(w.text) {
			return false
		}
		if w.expands && (len(refused) > 0 || name == "sed" || outsideHere(w.text)) {
			// A file name could be any flag, and a pattern from elsewhere could match anything.
			return false
		}
		for _, flag := range refused {
			if w.text == flag || strings.HasPrefix(w.text, flag+"=") || strings.HasPrefix(flag, "--") && strings.HasPrefix(w.text, flag) {
				return false
			}
			if short := strings.TrimPrefix(flag, "-"); len(short) == 1 && strings.HasPrefix(w.text, "-") && !strings.HasPrefix(w.text, "--") && strings.Contains(w.text, short) {
				return false // a cluster such as -bC, or -Cfile
			}
		}
	}
	if name == "sed" {
		quiet, script := false, ""
		for _, w := range words[1:] {
			switch {
			case w.text == "-n" || w.text == "--quiet" || w.text == "--silent":
				quiet = true
			case strings.HasPrefix(w.text, "-"):
				return false // -i, -e, -f and the rest
			case script == "":
				script = w.text
			}
		}
		return quiet && sedPrint.MatchString(script)
	}
	return true
}

// namesSecret is whether a word names a credential, by any of the paths that hold one.
func namesSecret(text string) bool {
	lower := strings.ToLower(text)
	for _, secret := range secrets {
		if strings.Contains(lower, secret) {
			return true
		}
	}
	return false
}

// outsideHere is whether a pattern reaches outside the working directory or into hidden files.
func outsideHere(pattern string) bool {
	return strings.HasPrefix(pattern, "~") || strings.HasPrefix(pattern, "/") ||
		strings.HasPrefix(pattern, ".") || strings.Contains(pattern, "/.")
}
