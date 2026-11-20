package imagegen

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// PyJSONDumps is json.dumps(value, ensure_ascii=False) with Python's default separators
// (", " and ": "). Map keys come out sorted (Go maps have no insertion order).
func PyJSONDumps(value any) string {
	var out strings.Builder
	writePyJSON(&out, value)
	return out.String()
}

func writePyJSON(out *strings.Builder, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteString(", ")
			}
			writePyJSON(out, key)
			out.WriteString(": ")
			writePyJSON(out, v[key])
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteString(", ")
			}
			writePyJSON(out, item)
		}
		out.WriteByte(']')
	default:
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.Encode(v)
		out.WriteString(strings.TrimRight(buf.String(), "\n"))
	}
}
