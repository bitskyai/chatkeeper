package search

import (
	"net/url"
	"strings"
	"testing"
)

func TestSharedDSNPreservesLocalPaths(t *testing.T) {
	for _, tc := range []struct{ path, want string }{{"/synthetic archive/index.db", "/synthetic archive/index.db"}, {"C:/synthetic archive/index.db", "/C:/synthetic archive/index.db"}} {
		dsn := sharedDSN(tc.path)
		u, e := url.Parse(dsn)
		if e != nil || u.Host != "" || u.Path != tc.want || u.Scheme != "file" || u.Query().Get("mode") != "rw" || !strings.Contains(dsn, "%20") {
			t.Fatal(dsn, u, e)
		}
	}
}
