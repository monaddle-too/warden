package policy

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Operation is one entry of the pinned GitHub REST catalog.
type Operation struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id"`
	Summary     string `json:"summary"`
}

type operationRoute struct {
	op      Operation
	pattern *regexp.Regexp
	names   []string
	params  int
}

// Operations matches request paths against the pinned catalog. Static paths
// win over parameterized ones, matching the API's routing model.
type Operations struct {
	Revision string
	routes   []operationRoute
}

var templateParameter = regexp.MustCompile(`\{[^{}]+\}`)
var nonWord = regexp.MustCompile(`\W`)

// LoadOperations reads vendor/github-operations.json.
func LoadOperations(path string) (*Operations, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document struct {
		Revision   string      `json:"revision"`
		Operations []Operation `json:"operations"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document.Revision == "" {
		return nil, errors.New("invalid operation catalog")
	}
	ops := &Operations{Revision: document.Revision}
	for _, item := range document.Operations {
		var pattern strings.Builder
		pattern.WriteString("^")
		var names []string
		last := 0
		for _, loc := range templateParameter.FindAllStringIndex(item.Path, -1) {
			pattern.WriteString(regexp.QuoteMeta(item.Path[last:loc[0]]))
			name := nonWord.ReplaceAllString(item.Path[loc[0]+1:loc[1]-1], "_")
			names = append(names, name)
			pattern.WriteString("([^/]+)")
			last = loc[1]
		}
		pattern.WriteString(regexp.QuoteMeta(item.Path[last:]))
		pattern.WriteString("$")
		compiled, err := regexp.Compile(pattern.String())
		if err != nil {
			return nil, err
		}
		ops.routes = append(ops.routes, operationRoute{op: item, pattern: compiled, names: names, params: strings.Count(item.Path, "{")})
	}
	sort.SliceStable(ops.routes, func(i, j int) bool { return ops.routes[i].params < ops.routes[j].params })
	return ops, nil
}

// Count returns the number of catalog routes.
func (o *Operations) Count() int { return len(o.routes) }

// Match returns the operation and path parameters for a method and path.
func (o *Operations) Match(method, path string) (*Operation, map[string]string) {
	for i := range o.routes {
		route := &o.routes[i]
		if route.op.Method != method {
			continue
		}
		m := route.pattern.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		params := map[string]string{}
		for j, name := range route.names {
			params[name] = m[j+1]
		}
		op := route.op
		return &op, params
	}
	return nil, map[string]string{}
}
