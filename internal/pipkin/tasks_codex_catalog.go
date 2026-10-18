package pipkin

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func codexTaskID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

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
		path := filepath.ToSlash(filepath.Join(home, catalog.name))
		if len(path) > 1 && path[1] == ':' {
			path = "/" + path
		}
		address := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(100)&_pragma=query_only(1)"}
		db, err := sql.Open("sqlite", address.String())
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
			if rows.Scan(&id, &source, &threadSource) != nil || !codexTaskID(id) || seen[id] {
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
