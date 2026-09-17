package main

// Where each assignment in an env file is, byte for byte.
//
// compose-go's dotenv parser (dotenv/parser.go) returns a map and forgets where
// anything was. To rewrite one variable's value in place, the writer needs the
// statements themselves, so this is that parser's loop ported line for line, with
// offsets kept. It must agree with compose-go on every file: which statements
// exist, which key each sets, where each value starts and ends — including a
// quoted value that spans lines, whose `KEY=` lines are content, not assignments.
// TestEnvScanAgreesWithComposeDotenv holds the two together.
//
// Deliberately NOT ported: value expansion (escapes, `${VAR}`). The writer only
// replaces a whole value token with a plain image reference or tag, and the
// project load that verifies every write interpolates with compose's own code.

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var (
	envUTF8BOM      = []byte("\uFEFF")
	envExportPrefix = regexp.MustCompile(`^export\s+`)
)

// envStatement is one statement of an env file, as compose-go's parser reads it.
type envStatement struct {
	key string
	// inherited: a bare `KEY` line, which takes KEY's value from the environment
	// (the process, or an earlier declaration) and has no value of its own.
	inherited bool
	quote     byte // 0 (unquoted), '\'' or '"'
	// valueStart, valueEnd delimit the value token in the file's bytes — inside
	// the quotes when quoted; before any ` #` comment and trailing space when not.
	valueStart, valueEnd int
}

// scanEnvStatements returns data's statements in file order.
func scanEnvStatements(data []byte) ([]envStatement, error) {
	base := 0
	if bytes.HasPrefix(data, envUTF8BOM) {
		base = len(envUTF8BOM) // compose-go seeks past a BOM before parsing
	}
	src := string(data[base:])
	line := 1
	var out []envStatement
	for {
		// getStatementStart
		start, ok := envStatementStart(src, &line)
		if !ok {
			return out, nil
		}
		base += start
		src = src[start:]

		// locateKeyName
		key, keyEnd, inherited, err := envLocateKey(src, line)
		if err != nil {
			return nil, err
		}
		if inherited && strings.IndexByte(key, ' ') == -1 {
			line++
		}
		key = strings.TrimRightFunc(key, unicode.IsSpace)
		if strings.Contains(key, " ") {
			return nil, fmt.Errorf("line %d: key cannot contain a space", line)
		}
		rest := src[keyEnd:]
		trimmed := strings.TrimLeftFunc(rest, envIsSpace)
		consumed := keyEnd + (len(rest) - len(trimmed))
		if inherited {
			out = append(out, envStatement{key: key, inherited: true})
			base += consumed
			src = trimmed
			continue
		}

		// extractVarValue
		st := envStatement{key: key}
		valueBase := base + consumed
		var next int // offset in trimmed where the next statement search resumes
		if quote := envQuote(trimmed); quote == 0 {
			value, _, found := strings.Cut(trimmed, "\n")
			line++
			value, _, _ = strings.Cut(value, " #")
			value = strings.TrimRightFunc(value, unicode.IsSpace)
			st.valueStart, st.valueEnd = valueBase, valueBase+len(value)
			if found {
				next = strings.IndexByte(trimmed, '\n') + 1
			} else {
				next = len(trimmed)
			}
		} else {
			end, err := envQuotedEnd(trimmed, quote, &line)
			if err != nil {
				return nil, err
			}
			st.quote = quote
			st.valueStart, st.valueEnd = valueBase+1, valueBase+end
			next = end + 1
		}
		out = append(out, st)
		base = valueBase + next
		src = trimmed[next:]
	}
}

// envStatementStart is getStatementStart: the offset of the next statement,
// skipping whitespace and comment lines.
func envStatementStart(src string, line *int) (int, bool) {
	off := 0
	for {
		pos := strings.IndexFunc(src[off:], func(r rune) bool {
			if r == '\n' {
				*line++
			}
			return !unicode.IsSpace(r)
		})
		if pos == -1 {
			return 0, false
		}
		off += pos
		if src[off] != '#' {
			return off, true
		}
		nl := strings.IndexByte(src[off:], '\n')
		if nl == -1 {
			return 0, false
		}
		off += nl
	}
}

// envLocateKey is locateKeyName up to the key's end: the raw key, the offset just
// past its separator, and whether the statement is a bare inherited `KEY`.
func envLocateKey(src string, line int) (key string, keyEnd int, inherited bool, err error) {
	if envExportPrefix.MatchString(src) {
		after := strings.TrimLeftFunc(strings.TrimPrefix(src, "export"), envIsSpace)
		keyEnd = len(src) - len(after)
		src = after
	}
	offset := 0
loop:
	for i, r := range src {
		if envIsSpace(r) {
			continue
		}
		switch r {
		case '=', ':', '\n':
			key = src[:i]
			offset = i + 1
			inherited = r == '\n'
			break loop
		case '_', '.', '-', '[', ']':
		default:
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				continue
			}
			return "", 0, false, fmt.Errorf("line %d: unexpected character %q in variable name %q",
				line, string(r), strings.Split(src, "\n")[0])
		}
	}
	if src == "" {
		return "", 0, false, fmt.Errorf("zero length string")
	}
	return key, keyEnd + offset, inherited, nil
}

func envQuote(src string) byte {
	if src != "" && (src[0] == '"' || src[0] == '\'') {
		return src[0]
	}
	return 0
}

// envQuotedEnd is the quoted half of extractVarValue: the offset of the closing
// quote, honouring backslash escapes.
func envQuotedEnd(src string, quote byte, line *int) (int, error) {
	escaped := false
	for i := 1; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			*line++
		}
		if c != quote {
			if !escaped && c == '\\' {
				escaped = true
				continue
			}
			escaped = false
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		return i, nil
	}
	end := strings.IndexByte(src, '\n')
	if end == -1 {
		end = len(src)
	}
	return 0, fmt.Errorf("line %d: unterminated quoted value %s", *line, src[:end])
}

// envIsSpace is dotenv's isSpace: whitespace other than a line break.
func envIsSpace(r rune) bool {
	switch r {
	case '\t', '\v', '\f', '\r', ' ', 0x85, 0xA0:
		return true
	}
	return false
}
