package tracker

import "encoding/json"

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// scopedQueries takes a base search query and returns one or more queries
// with " repo:owner/name" qualifiers appended, batched to stay under maxSearchQueryLen.
// If no repos are configured, returns the base query as-is.
func scopedQueries(base string, repos []string) []string {
	if len(repos) == 0 {
		return []string{base}
	}

	var queries []string
	current := base
	for _, repo := range repos {
		term := " repo:" + repo
		if len(current)+len(term) > maxSearchQueryLen {
			queries = append(queries, current)
			current = base + term
		} else {
			current += term
		}
	}
	queries = append(queries, current)
	return queries
}
