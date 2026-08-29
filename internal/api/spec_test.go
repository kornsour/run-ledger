package api

// This file checks docs/openapi.yaml against the code it claims to
// describe, so a route or a JSON field that drifts between the two fails
// the build instead of shipping as a stale spec.
//
// It deliberately does not attempt to reconstruct the full spec from
// reflection -- that would just move the hand-maintained part from the
// yaml into this file. Instead it checks the two properties most likely to
// silently drift and easiest to state precisely: the set of routes, and
// the set of JSON field names each request/response type actually carries.

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
	"github.com/kornsour/run-ledger/internal/spread"
)

const specPath = "../../docs/openapi.yaml"

type oaOperation struct {
	OperationID string `yaml:"operationId"`
}

type oaSpec struct {
	Paths      map[string]map[string]oaOperation `yaml:"paths"`
	Components struct {
		Schemas map[string]oaSchema `yaml:"schemas"`
	} `yaml:"components"`
}

type oaSchema struct {
	Ref        string              `yaml:"$ref"`
	AllOf      []oaSchema          `yaml:"allOf"`
	Properties map[string]oaSchema `yaml:"properties"`
}

func loadSpec(t *testing.T) oaSpec {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec oaSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	return spec
}

// TestRoutesMatchSpec checks that every route this server registers has a
// matching path+method in the spec, and vice versa.
func TestRoutesMatchSpec(t *testing.T) {
	spec := loadSpec(t)

	inCode := map[string]bool{}
	for _, rt := range (&Server{}).routes() {
		inCode[rt.pattern] = true
	}

	inSpec := map[string]bool{}
	for path, methods := range spec.Paths {
		for method := range methods {
			inSpec[strings.ToUpper(method)+" "+path] = true
		}
	}

	for pattern := range inCode {
		if !inSpec[pattern] {
			t.Errorf("route %q is registered in Server.routes but missing from %s", pattern, specPath)
		}
	}
	for pattern := range inSpec {
		if !inCode[pattern] {
			t.Errorf("route %q is documented in %s but not registered in Server.routes", pattern, specPath)
		}
	}
}

// flattenSchema resolves $ref and allOf to the flat set of property names a
// schema describes. Nested property schemas are not recursed into --
// top-level field names are what a Go struct's json tags can be compared
// against directly.
func flattenSchema(name string, spec oaSpec, seen map[string]bool) map[string]bool {
	if seen[name] {
		return nil
	}
	seen[name] = true
	s, ok := spec.Components.Schemas[name]
	if !ok {
		return nil
	}
	names := map[string]bool{}
	for _, sub := range s.AllOf {
		if sub.Ref != "" {
			for k := range flattenSchema(strings.TrimPrefix(sub.Ref, "#/components/schemas/"), spec, seen) {
				names[k] = true
			}
			continue
		}
		for k := range sub.Properties {
			names[k] = true
		}
	}
	for k := range s.Properties {
		names[k] = true
	}
	return names
}

// jsonFieldNames returns the JSON field names t's encoding/json tags
// produce -- the exact set json.Decoder.DisallowUnknownFields accepts, or
// json.Encoder emits.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			names[f.Name] = true
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names[name] = true
	}
	return names
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSchemasMatchGoTypes checks that each named schema in the spec that
// corresponds to a concrete request/response type declares exactly the
// JSON field names that type's encoding/json tags actually produce or
// accept. Schemas backed by an ad hoc map (handlers that build a response
// with map[string]any rather than a named struct) are checked against a
// literal key set instead, since there is no type to reflect on.
func TestSchemasMatchGoTypes(t *testing.T) {
	cases := []struct {
		schema string
		typ    reflect.Type
		keys   []string
	}{
		{schema: "RunInput", typ: reflect.TypeOf(lineage.Run{})},
		{schema: "Run", typ: reflect.TypeOf(lineage.Run{})},
		{schema: "RunPatch", typ: reflect.TypeOf(patchRequest{})},
		{schema: "RunPage", keys: []string{"runs", "count", "limit", "next_cursor"}},
		{schema: "CompareField", typ: reflect.TypeOf(compare.Field{})},
		{schema: "CompareResult", typ: reflect.TypeOf(compare.Result{})},
		{schema: "MetricStat", typ: reflect.TypeOf(spread.MetricStat{})},
		{schema: "ProvenanceDiff", typ: reflect.TypeOf(spread.ProvenanceDiff{})},
		{schema: "SpreadGroup", typ: reflect.TypeOf(spread.Group{})},
		{schema: "SpreadGroupList", keys: []string{"groups", "count", "limit", "next_cursor"}},
	}

	spec := loadSpec(t)

	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			if _, ok := spec.Components.Schemas[c.schema]; !ok {
				t.Fatalf("schema %q not found in %s", c.schema, specPath)
			}
			wantNames := flattenSchema(c.schema, spec, map[string]bool{})

			var gotNames map[string]bool
			if c.typ != nil {
				gotNames = jsonFieldNames(c.typ)
			} else {
				gotNames = map[string]bool{}
				for _, k := range c.keys {
					gotNames[k] = true
				}
			}

			for name := range gotNames {
				if !wantNames[name] {
					t.Errorf("field %q is present in the Go type but missing from schema %q in %s", name, c.schema, specPath)
				}
			}
			for name := range wantNames {
				if !gotNames[name] {
					t.Errorf("schema %q in %s declares field %q, which the Go type does not have", c.schema, specPath, name)
				}
			}
			if t.Failed() {
				t.Logf("go fields:   %v", sortedKeys(gotNames))
				t.Logf("spec fields: %v", sortedKeys(wantNames))
			}
		})
	}
}
