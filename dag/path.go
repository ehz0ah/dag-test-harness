package dag

import (
	"reflect"
	"strconv"
	"strings"
)

// extractPath walks a dotted field path into a value (the glom equivalent used by
// Ref.At / VariableSpec). Each segment is a map key or, if it parses as an
// integer, a slice/array index. A miss (absent key, out-of-range index, wrong
// kind) yields nil rather than an error, matching the kernel's tolerant access.
func extractPath(v any, path string) any {
	if path == "" {
		return v
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		if cur == nil {
			return nil
		}
		if m, ok := cur.(map[string]any); ok {
			cur = m[seg]
			continue
		}
		cur = reflectStep(cur, seg)
	}
	return cur
}

func reflectStep(cur any, seg string) any {
	rv := reflect.ValueOf(cur)
	switch rv.Kind() {
	case reflect.Map:
		mv := rv.MapIndex(reflect.ValueOf(seg))
		if !mv.IsValid() {
			return nil
		}
		return mv.Interface()
	case reflect.Slice, reflect.Array:
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= rv.Len() {
			return nil
		}
		return rv.Index(i).Interface()
	case reflect.Struct:
		fv := rv.FieldByName(seg)
		if !fv.IsValid() {
			return nil
		}
		return fv.Interface()
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return reflectStep(rv.Elem().Interface(), seg)
	}
	return nil
}
