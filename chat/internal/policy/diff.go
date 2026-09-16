package policy

import (
	"fmt"
	"strings"
)

// splitKeepEnds mirrors str.splitlines(keepends=True) for '\n' endings.
func splitKeepEnds(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

type diffOp struct {
	kind   byte // '=', '-', '+'
	a, b   int
	length int
}

// lcsOps computes an edit script by longest common subsequence.
func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// Strip common prefix/suffix to keep the table small for typical edits.
	prefix := 0
	for prefix < n && prefix < m && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < n-prefix && suffix < m-prefix && a[n-1-suffix] == b[m-1-suffix] {
		suffix++
	}
	core := a[prefix : n-suffix]
	other := b[prefix : m-suffix]
	table := make([][]int, len(core)+1)
	for i := range table {
		table[i] = make([]int, len(other)+1)
	}
	for i := len(core) - 1; i >= 0; i-- {
		for j := len(other) - 1; j >= 0; j-- {
			if core[i] == other[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	var ops []diffOp
	push := func(kind byte, ai, bi int) {
		if len(ops) > 0 {
			last := &ops[len(ops)-1]
			if last.kind == kind {
				last.length++
				return
			}
		}
		ops = append(ops, diffOp{kind: kind, a: ai, b: bi, length: 1})
	}
	for k := 0; k < prefix; k++ {
		push('=', k, k)
	}
	i, j := 0, 0
	for i < len(core) && j < len(other) {
		switch {
		case core[i] == other[j]:
			push('=', prefix+i, prefix+j)
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			push('-', prefix+i, prefix+j)
			i++
		default:
			push('+', prefix+i, prefix+j)
			j++
		}
	}
	for ; i < len(core); i++ {
		push('-', prefix+i, prefix+j)
	}
	for ; j < len(other); j++ {
		push('+', prefix+i, prefix+j)
	}
	for k := 0; k < suffix; k++ {
		push('=', n-suffix+k, m-suffix+k)
	}
	return ops
}

// UnifiedDiff renders a unified diff with three lines of context in the
// same shape as difflib.unified_diff (lines keep their own endings).
func UnifiedDiff(a, b []string, fromFile, toFile string) []string {
	ops := lcsOps(a, b)
	changed := false
	for _, op := range ops {
		if op.kind != '=' {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	const context = 3
	// Expand ops into per-line entries for hunk grouping.
	type entry struct {
		kind byte
		ai   int
		bi   int
	}
	var entries []entry
	for _, op := range ops {
		for k := 0; k < op.length; k++ {
			switch op.kind {
			case '=':
				entries = append(entries, entry{'=', op.a + k, op.b + k})
			case '-':
				entries = append(entries, entry{'-', op.a + k, op.b})
			case '+':
				entries = append(entries, entry{'+', op.a, op.b + k})
			}
		}
	}
	out := []string{"--- " + fromFile + "\n", "+++ " + toFile + "\n"}
	i := 0
	for i < len(entries) {
		if entries[i].kind == '=' {
			i++
			continue
		}
		start := i - context
		if start < 0 {
			start = 0
		}
		end := i
		for end < len(entries) {
			if entries[end].kind != '=' {
				end++
				continue
			}
			// Count equal run; stop the hunk if it exceeds 2*context.
			run := end
			for run < len(entries) && entries[run].kind == '=' {
				run++
			}
			if run-end > 2*context || run == len(entries) {
				end += context
				if end > run {
					end = run
				}
				break
			}
			end = run
		}
		if end > len(entries) {
			end = len(entries)
		}
		aStart, bStart := 0, 0
		aLen, bLen := 0, 0
		first := true
		var body []string
		for _, e := range entries[start:end] {
			if first {
				aStart, bStart = e.ai, e.bi
				first = false
			}
			switch e.kind {
			case '=':
				body = append(body, " "+a[e.ai])
				aLen++
				bLen++
			case '-':
				body = append(body, "-"+a[e.ai])
				aLen++
			case '+':
				body = append(body, "+"+b[e.bi])
				bLen++
			}
		}
		out = append(out, fmt.Sprintf("@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen)))
		out = append(out, body...)
		i = end
	}
	return out
}

func hunkRange(start, length int) string {
	begin := start + 1
	if length == 0 {
		begin = start
	}
	if length == 1 {
		return fmt.Sprintf("%d", begin)
	}
	return fmt.Sprintf("%d,%d", begin, length)
}
