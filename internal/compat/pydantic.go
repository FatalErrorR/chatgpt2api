package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// pydanticBool follows pydantic v2 lax bool parsing: true/false, 0/1, and the usual words.
func pydanticBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case json.Number:
		switch v.String() {
		case "0", "0.0":
			return false, true
		case "1", "1.0":
			return true, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "on", "t", "true", "y", "yes":
			return true, true
		case "0", "off", "f", "false", "n", "no":
			return false, true
		}
	}
	return false, false
}

// pydanticInt follows pydantic v2 lax int parsing: ints, whole floats and numeric strings.
func pydanticInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
		if f, err := v.Float64(); err == nil && f == float64(int64(f)) {
			return int(f), true
		}
	case string:
		text := strings.TrimSpace(v)
		var n int
		if _, err := fmt.Sscanf(text, "%d", &n); err == nil && fmt.Sprint(n) == strings.TrimPrefix(text, "+") {
			return n, true
		}
	case bool:
		return 0, false
	}
	return 0, false
}
