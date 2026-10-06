package app

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
)

// SelectPrefix selects tools by exact name: "select:name1,name2".
const SelectPrefix = "select:"

// SearchResult is the outcome of SearchTools.
type SearchResult struct {
	// Tools are the matching tools, best match first.
	Tools []mcp.Tool
	// Total is the number of tools that matched before limiting.
	Total int
	// Missing lists names given to "select:" that do not exist.
	Missing []string
}

// SearchTools finds tools matching query, mirroring the query forms of
// Claude Code's ToolSearch:
//
//   - "select:a,b" returns the named tools exactly, in the given order.
//   - "+term" requires term to appear in the tool name.
//   - other terms rank tools by matches in the name, description and
//     parameter names.
//
// An empty query matches every tool. maxResults <= 0 means no limit.
func SearchTools(tools []mcp.Tool, query string, maxResults int) SearchResult {
	query = strings.TrimSpace(query)
	if names, ok := strings.CutPrefix(query, SelectPrefix); ok {
		return selectTools(tools, names)
	}

	var required, optional []string
	for term := range strings.FieldsSeq(strings.ToLower(query)) {
		if t, ok := strings.CutPrefix(term, "+"); ok {
			if t != "" {
				required = append(required, t)
			}
			continue
		}
		optional = append(optional, term)
	}

	type scored struct {
		tool  mcp.Tool
		score int
	}
	var matches []scored
	for _, t := range tools {
		doc := newToolDoc(t)
		if !doc.nameHasAll(required) {
			continue
		}
		score := 0
		for _, term := range optional {
			score += doc.score(term)
		}
		if len(optional) > 0 && score == 0 {
			continue
		}
		for _, term := range required {
			score += doc.score(term)
		}
		matches = append(matches, scored{t, score})
	}
	slices.SortStableFunc(matches, func(a, b scored) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(a.tool.Name, b.tool.Name)
	})

	res := SearchResult{Total: len(matches)}
	for i, m := range matches {
		if maxResults > 0 && i >= maxResults {
			break
		}
		res.Tools = append(res.Tools, m.tool)
	}
	return res
}

func selectTools(tools []mcp.Tool, names string) SearchResult {
	var res SearchResult
	for name := range strings.SplitSeq(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if t, ok := findTool(tools, name); ok {
			res.Tools = append(res.Tools, t)
		} else {
			res.Missing = append(res.Missing, name)
		}
	}
	res.Total = len(res.Tools)
	return res
}

// toolDoc is the lowercased searchable text of a tool.
type toolDoc struct {
	name      string
	nameWords []string
	desc      string
	params    []string
}

func newToolDoc(t mcp.Tool) toolDoc {
	d := toolDoc{
		name:      strings.ToLower(t.Name),
		nameWords: splitWords(t.Name),
		desc:      strings.ToLower(t.Title + " " + t.Description),
	}
	for p := range t.InputSchema.Properties {
		d.params = append(d.params, strings.ToLower(p))
	}
	return d
}

func (d toolDoc) nameHasAll(terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(d.name, t) {
			return false
		}
	}
	return true
}

func (d toolDoc) score(term string) int {
	score := 0
	switch {
	case d.name == term:
		score += 10
	case slices.Contains(d.nameWords, term):
		score += 5
	case strings.Contains(d.name, term):
		score += 3
	}
	if strings.Contains(d.desc, term) {
		score += 2
	}
	for _, p := range d.params {
		if strings.Contains(p, term) {
			score++
			break
		}
	}
	return score
}

// splitWords splits an identifier on non-alphanumerics and camelCase
// boundaries: "getHTTPStatus_v2" -> [get http status v2].
func splitWords(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && len(cur) > 0 {
			prevLower := unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || (unicode.IsUpper(runes[i-1]) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}
