package catalog

import (
	"strconv"
	"strings"
	"testing"
)

func TestPostgresQSurferFourFieldSearchBindsEveryPlaceholder(t *testing.T) {
	where, args, err := postgresMatchWhere("office", []string{"name", "path", "extension", "content"}, "prefix", BooleanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	query := `SELECT d.id FROM documents d WHERE d.status = 'active' AND ` + where +
		` ORDER BY ts_rank_cd(d.search_vector, to_tsquery('simple', ?)) DESC, d.normalized_path ASC LIMIT ? OFFSET ?`
	args = append(args, postgresRankQuery("office"), 11, 0)
	if got := postgresPlaceholderCount(postgresPlaceholders(query)); got != len(args) {
		t.Fatalf("PostgreSQL placeholder count = %d, args = %d\nquery: %s", got, len(args), query)
	}
}

func postgresPlaceholderCount(query string) int {
	max := 0
	for _, field := range strings.FieldsFunc(query, func(r rune) bool {
		return r != '$' && (r < '0' || r > '9')
	}) {
		if strings.HasPrefix(field, "$") {
			if value, err := strconv.Atoi(strings.TrimPrefix(field, "$")); err == nil && value > max {
				max = value
			}
		}
	}
	return max
}
