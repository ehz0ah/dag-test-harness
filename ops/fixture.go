package ops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"code.byted.org/data-arch/ovtest/dag"
)

var (
	fixtureServersMu sync.Mutex
	fixtureServers   []*httptest.Server
)

func CloseFixtureServers() {
	fixtureServersMu.Lock()
	servers := fixtureServers
	fixtureServers = nil
	fixtureServersMu.Unlock()

	for _, server := range servers {
		server.Close()
	}
}

func fixtureFileOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"after"}, Outputs: []string{"path", "content"}}, false, func(b *base) execFn {
		return func(map[string]any) (map[string]any, error) {
			content, err := b.needStr("content")
			if err != nil {
				return nil, err
			}
			filePath := asString(b.oc["path"])
			if filePath == "" {
				name := firstNonEmpty(asString(b.oc["name"]), "resource.txt")
				filePath = filepath.Join(os.TempDir(), "ovtest-fixtures", name)
			}
			filePath, err = filepath.Abs(filePath)
			if err != nil {
				return nil, configErr(b.name, "could not resolve fixture file path: "+err.Error())
			}
			if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
				return nil, configErr(b.name, "could not create fixture file directory: "+err.Error())
			}
			if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
				return nil, configErr(b.name, "could not write fixture file: "+err.Error())
			}
			return map[string]any{"path": filePath, "content": content}, nil
		}
	})
}

func fixtureServerOp() dag.Factory {
	return factory(dag.Meta{Inputs: []string{"after"}, Outputs: []string{"url", "content"}}, false, func(b *base) execFn {
		return func(map[string]any) (map[string]any, error) {
			content, err := b.needStr("content")
			if err != nil {
				return nil, err
			}
			route := asString(b.oc["path"])
			if route == "" {
				route = "/resource.txt"
			}
			if !strings.HasPrefix(route, "/") {
				route = "/" + route
			}
			contentType := firstNonEmpty(asString(b.oc["content_type"]), "text/markdown; charset=utf-8")
			mux := http.NewServeMux()
			mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
				if path.Clean(r.URL.Path) != path.Clean(route) {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", contentType)
				_, _ = fmt.Fprint(w, content)
			})
			server := httptest.NewServer(mux)
			fixtureServersMu.Lock()
			fixtureServers = append(fixtureServers, server)
			fixtureServersMu.Unlock()
			return map[string]any{"url": server.URL + route, "content": content}, nil
		}
	})
}
