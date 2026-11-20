package settings

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// The helpers below copy Python's conversion rules, because config.json values were written by the
// Python version and the same input must give the same setting in both.

// pyInt mimics Python's int(value): floats truncate, strings must be a whole integer, bools are 0/1.
func pyInt(value any) (int, bool) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return pyInt(f)
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int(v), true
	case string:
		text := strings.TrimSpace(strings.ReplaceAll(v, "_", ""))
		if text == "" || strings.HasPrefix(text, "_") {
			return 0, false
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// pyFloat mimics Python's float(value).
func pyFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case string:
		text := strings.TrimSpace(v)
		lowered := strings.ToLower(text)
		if lowered == "nan" || lowered == "inf" || lowered == "-inf" || lowered == "infinity" || lowered == "-infinity" || lowered == "+inf" {
			f, _ := strconv.ParseFloat(lowered, 64)
			return f, true
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// pyTruthy mimics Python's bool(value).
func pyTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case bool:
		return v
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

var trueWords = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
var falseWords = map[string]bool{"0": true, "false": true, "no": true, "off": true}

// normalizeBool is services/config.py _normalize_bool.
func normalizeBool(value any, def bool) bool {
	switch v := value.(type) {
	case string:
		lowered := strings.ToLower(strings.TrimSpace(v))
		if trueWords[lowered] {
			return true
		}
		if falseWords[lowered] {
			return false
		}
		return def
	case nil:
		return def
	}
	return pyTruthy(value)
}

// looseBool is the older pattern in config.py: strings count only when they are a "true" word,
// anything else uses Python truthiness.
func looseBool(value any) bool {
	if text, ok := value.(string); ok {
		return trueWords[strings.ToLower(strings.TrimSpace(text))]
	}
	return pyTruthy(value)
}

// normalizePositiveInt is services/config.py _normalize_positive_int.
func normalizePositiveInt(value any, def int, minimum int) int {
	n, ok := pyInt(value)
	if !ok {
		n = def
	}
	if n < minimum {
		return minimum
	}
	return n
}

// pyStr mimics str(value or "").strip(): falsy values become "".
func pyStr(value any) string {
	if !pyTruthy(value) {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case bool:
		if v {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(v)
	case json.Number:
		// JSON keeps the literal: 5 stays "5", 5.0 stays "5.0", as Python's str() gives
		return v.String()
	case float64:
		return strings.TrimSpace(formatPyFloat(v))
	}
	return ""
}

func formatPyFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e16 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func asMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
