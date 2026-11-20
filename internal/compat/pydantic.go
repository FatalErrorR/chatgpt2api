package compat

import (
	"encoding/json"
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
