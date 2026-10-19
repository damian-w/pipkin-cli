package pipkin

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The catalog only suggests threads; the running app must confirm each one.
func codexTaskCandidates(ctx context.Context, home string, limit int) []string {
	if ctx.Err() != nil || limit <= 0 {
		return nil
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil
	}
	type catalog struct {
		name    string
		version int
	}
	var catalogs []catalog
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasPrefix(name, "state_") && strings.HasSuffix(name, ".sqlite") {
			version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "state_"), ".sqlite"))
			if err == nil && version > 0 {
				catalogs = append(catalogs, catalog{name, version})
			}
		}
	}
	sort.Slice(catalogs, func(i, j int) bool { return catalogs[i].version > catalogs[j].version })
	seen := map[string]bool{}
	result := []string{}
	for i, catalog := range catalogs {
		if i >= 4 || ctx.Err() != nil || len(result) >= limit {
			break
		}
		db, err := openSQLiteReadOnly(filepath.Join(home, catalog.name), "mode=ro&_pragma=busy_timeout(100)&_pragma=query_only(1)")
		if err != nil {
			continue
		}
		db.SetMaxOpenConns(1)
		rows, err := db.QueryContext(ctx, "SELECT id,source,thread_source FROM threads WHERE archived=0 ORDER BY updated_at_ms DESC LIMIT ?", limit*4)
		if err != nil {
			db.Close()
			continue
		}
		for rows.Next() && len(result) < limit {
			var id, source string
			var threadSource sql.NullString
			if rows.Scan(&id, &source, &threadSource) != nil || !validUUID(id) || seen[id] {
				continue
			}
			if source != "vscode" && source != `"vscode"` || !codexTaskThreadSource(threadSource.String) {
				continue
			}
			seen[id] = true
			result = append(result, id)
		}
		rows.Close()
		db.Close()
	}
	return result
}
