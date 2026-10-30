package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Read-only commands are allowed without the reviewer: a shell command each of whose parts (split
// on ; && || | & and newlines) is a command that only reads, with nothing that writes or runs
// something else. Devin asks for these when one part is missing from its own allow list, as in
// `sleep 9; tail -4 log`, and they are most of what it asks for while it waits on a build.

// readers are the commands taken as only reading, each with the arguments that would make it do
// more, refused.
var readers = map[string][]string{
	"cat": nil, "head": nil, "tail": nil, "less": nil, "wc": nil, "nl": nil, "tac": nil,
	"ls": nil, "stat": nil, "file": nil, "du": nil, "df": nil, "tree": nil,
	"grep": nil, "egrep": nil, "fgrep": nil,
	"rg":   {"--pre", "--pre-glob"},
	"find": {"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"},
	"sort": {"-o", "--output"},
	"uniq": nil, "cut": nil, "tr": nil, "diff": nil, "cmp": nil, "jq": nil, "column": nil,
	"echo": nil, "printf": nil, "sleep": nil, "true": nil, "false": nil, "test": nil, "[": nil,
	"pwd": nil, "cd": nil, "date": nil, "which": nil, "whoami": nil, "uname": nil, "id": nil,
	"basename": nil, "dirname": nil, "realpath": nil, "readlink": nil,
	"pgrep": nil, "ps": nil, "nproc": nil, "free": nil, "uptime": nil,
	"git": nil, // only the subcommands in gitReaders
	"sed": nil, // only printing lines: see sedPrints
}

// sedPrint is a sed script that only prints lines by number, as `sed -n '560,640p;700p'`.
var sedPrint = regexp.MustCompile(`^(\d+|\$)(,(\d+|\$))?p(;(\d+|\$)(,(\d+|\$))?p)*;?$`)

var gitReaders = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "rev-parse": true, "ls-files": true,
	"blame": true, "describe": true, "shortlog": true, "rev-list": true, "cat-file": true,
}

// readOnly is whether a tool call is a shell command that only reads.
func readOnly(call map[string]any) bool {
	if kind, _ := call["kind"].(string); kind != "execute" {
		return false
	}
	input, _ := call["rawInput"].(map[string]any)
	command, _ := input["command"].(string)
	if strings.TrimSpace(command) == "" {
		return false
	}
	parts, ok := shellParts(command)
	if !ok {
		return false
	}
	for _, words := range parts {
		if !readerCall(words) {
			return false
		}
	}
	return true
}

// readerCall is whether one simple command, as words, only reads.
func readerCall(words []string) bool {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return true
	}
	name := filepath.Base(words[0])
	refused, known := readers[name]
	if !known {
		return false
	}
	for _, word := range words[1:] {
		for _, flag := range refused {
			if word == flag || strings.HasPrefix(word, flag+"=") {
				return false
			}
		}
	}
	if name == "sed" {
		quiet, script := false, ""
		for _, arg := range words[1:] {
			switch {
			case arg == "-n" || arg == "--quiet" || arg == "--silent":
				quiet = true
			case strings.HasPrefix(arg, "-"):
				return false // -i, -e, -f and the rest
			case script == "":
				script = arg
			}
		}
		return quiet && sedPrint.MatchString(script)
	}
	if name == "git" {
		args := words[1:]
		for len(args) >= 2 && args[0] == "-C" {
			args = args[2:]
		}
		if len(args) == 0 || !gitReaders[args[0]] {
			return false
		}
		for _, arg := range args {
			if arg == "--output" || strings.HasPrefix(arg, "--output=") || strings.HasPrefix(arg, "--ext-diff") {
				return false
			}
		}
	}
	return true
}

func isAssignment(word string) bool {
	name, _, found := strings.Cut(word, "=")
	if !found || name == "" {
		return false
	}
	for _, c := range name {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// shellParts splits a command into its simple commands' words, honouring quotes. It refuses (ok
// false) what it cannot see through or what writes: command substitution, process substitution,
// backticks, and a redirect to anywhere but /dev/null or another descriptor.
func shellParts(command string) (parts [][]string, ok bool) {
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endPart := func() {
		endWord()
		if len(words) > 0 {
			parts = append(parts, words)
			words = nil
		}
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				j++
			}
			if j == len(runes) {
				return nil, false
			}
			word.WriteString(string(runes[i+1 : j]))
			inWord, i = true, j
		case c == '"':
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' {
					j++
				} else if runes[j] == '`' || runes[j] == '$' && j+1 < len(runes) && runes[j+1] == '(' {
					return nil, false
				}
				j++
			}
			if j >= len(runes) {
				return nil, false
			}
			word.WriteString(string(runes[i+1 : j]))
			inWord, i = true, j
		case c == '\\' && i+1 < len(runes):
			word.WriteRune(runes[i+1])
			inWord = true
			i++
		case c == '`', c == '$' && i+1 < len(runes) && runes[i+1] == '(':
			return nil, false
		case (c == '<' || c == '>') && i+1 < len(runes) && runes[i+1] == '(':
			return nil, false
		case c == '>':
			// A descriptor before it (2>) is part of the redirect, not a word.
			if inWord && strings.Trim(word.String(), "0123456789&") == "" {
				word.Reset()
				inWord = false
			}
			endWord()
			j := i + 1
			for j < len(runes) && (runes[j] == '>' || runes[j] == '|') {
				j++
			}
			for j < len(runes) && runes[j] == ' ' {
				j++
			}
			k := j
			for k < len(runes) && !strings.ContainsRune(" \t\n;&|<>", runes[k]) {
				k++
			}
			target := string(runes[j:k])
			if j < len(runes) && runes[j] == '&' {
				k = j + 1
				for k < len(runes) && runes[k] >= '0' && runes[k] <= '9' {
					k++
				}
				target = "&"
			}
			if target != "/dev/null" && target != "&" {
				return nil, false
			}
			i = k - 1
		case c == '&' && i+1 < len(runes) && runes[i+1] == '>':
			// &> /dev/null: the > case reads the target.
			endWord()
		case c == ';' || c == '&' || c == '|' || c == '\n':
			endPart()
		case c == ' ' || c == '\t':
			endWord()
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	endPart()
	return parts, true
}
