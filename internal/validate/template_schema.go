package validate

import (
	"reflect"
	"sort"
	"strings"

	"github.com/Lincyaw/workbuddy/internal/runtime"
)

// FieldKind classifies an entry in the rendered template-field schema.
//
//   - FieldKindScalar — a leaf value (string, int, bool, time.Duration, …).
//   - FieldKindStruct — a nested struct exposed for further dotted paths.
//   - FieldKindSlice  — a slice/array; the iteration form `path[]…` exposes
//     fields of its element type.
type FieldKind string

// FieldKind values.
const (
	FieldKindScalar FieldKind = "scalar"
	FieldKindStruct FieldKind = "struct"
	FieldKindSlice  FieldKind = "slice"
)

// FieldSchema maps dotted paths (e.g. `Issue.Comments[].Author`) to their
// FieldKind. Slices use `[]` notation between segments to indicate that
// the surrounding scope is the slice element. The empty-string key (root)
// is intentionally not included.
type FieldSchema map[string]FieldKind

// BuildTaskContextSchema reflects over runtime.TaskContext to produce the
// canonical template-field schema. Stable across runs (paths are sorted
// when serialized for golden files).
func BuildTaskContextSchema() FieldSchema {
	schema := FieldSchema{}
	walkTypeIntoSchema(reflect.TypeOf(runtime.TaskContext{}), "", schema, map[reflect.Type]bool{})
	return schema
}

// walkTypeIntoSchema descends into t, recording each exported field at
// `prefix.Name`. Slices register both `prefix.Name` (FieldKindSlice) and
// — for slices of structs — `prefix.Name[].<sub>` for each element field.
//
// `seen` guards against infinite recursion if the type graph ever becomes
// cyclic (currently it isn't, but cheap insurance).
func walkTypeIntoSchema(t reflect.Type, prefix string, out FieldSchema, seen map[reflect.Type]bool) {
	if t == nil {
		return
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	if seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		path := joinPath(prefix, f.Name)
		ft := f.Type
		// Unwrap pointers; treat nil-able structs like their value form.
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		switch ft.Kind() {
		case reflect.Struct:
			if isOpaqueStruct(ft) {
				out[path] = FieldKindScalar
				continue
			}
			out[path] = FieldKindStruct
			walkTypeIntoSchema(ft, path, out, seen)
		case reflect.Slice, reflect.Array:
			out[path] = FieldKindSlice
			elem := ft.Elem()
			for elem.Kind() == reflect.Ptr {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct && !isOpaqueStruct(elem) {
				walkTypeIntoSchema(elem, path+"[]", out, seen)
			}
		default:
			out[path] = FieldKindScalar
		}
	}
}

// isOpaqueStruct decides whether a struct type should be exposed as a
// scalar (no further drill-down) when the validator encounters it. Used
// for `time.Time`, `time.Duration`, `json.RawMessage` and similar leaf-
// like types that callers shouldn't dotted-path into from templates.
func isOpaqueStruct(t reflect.Type) bool {
	switch t.PkgPath() + "." + t.Name() {
	case "time.Time", "time.Duration", "encoding/json.RawMessage":
		return true
	}
	return false
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	if strings.HasSuffix(prefix, "[]") {
		return prefix + "." + name
	}
	return prefix + "." + name
}

// SortedPaths returns the schema paths in deterministic order. Used by
// the golden snapshot test.
func (s FieldSchema) SortedPaths() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
