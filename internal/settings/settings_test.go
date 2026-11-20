package settings

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type vector struct {
	Name     string         `json:"name"`
	Config   map[string]any `json:"config"`
	Get      map[string]any `json:"get"`
	KeepFrom string         `json:"keep_from_2026_11_20"`
	Update   map[string]any `json:"update"`
	Result   map[string]any `json:"result"`
	Saved    map[string]any `json:"saved"`
	Error    *string        `json:"error"`
}

// roundTrip makes Go values comparable with decoded JSON (ints and floats both become float64).
func roundTrip(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func diffKeys(t *testing.T, got, want map[string]any) {
	t.Helper()
	want = roundTrip(t, want).(map[string]any)
	for key := range want {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("  %s: got %#v, want %#v", key, got[key], want[key])
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("  %s: only in Go (%#v)", key, got[key])
		}
	}
}

// TestMatchesPythonConfigStore replays the golden vectors produced by tools/vectors/settings_vectors.py
// against services/config.py on main.
func TestMatchesPythonConfigStore(t *testing.T) {
	t.Setenv("CHATGPT2API_AUTH_KEY", "vector-key")
	for _, name := range []string{"CHATGPT2API_BASE_URL", "CHATGPT2API_SENTINEL_SOLVER_URL", "CHATGPT2API_SENTINEL_SOLVER_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "python_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []vector
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // config inputs keep int vs float, like config.json read by Load
	if err := decoder.Decode(&vectors); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			if v.Update == nil {
				store := NewForTest(v.Config, "")
				got := roundTrip(t, store.Get()).(map[string]any)
				diffKeys(t, got, v.Get)
				if keep := store.ImageRetentionKeepFrom(day); keep != v.KeepFrom {
					t.Errorf("keep_from: got %s, want %s", keep, v.KeepFrom)
				}
				return
			}
			path := filepath.Join(t.TempDir(), "config.json")
			store := NewForTest(v.Config, path)
			result, err := store.Update(v.Update)
			if v.Error != nil {
				if err == nil || err.Error() != *v.Error || !IsValidationError(err) {
					t.Fatalf("want validation error %q, got %v", *v.Error, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			diffKeys(t, roundTrip(t, result).(map[string]any), v.Result)
			var saved map[string]any
			data, _ := os.ReadFile(path)
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			diffKeys(t, saved, v.Saved)
		})
	}
}

func TestAuthKeyFromEnvOrFile(t *testing.T) {
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	os.Unsetenv("CHATGPT2API_AUTH_KEY")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if _, err := Load(path, dir); err != ErrMissingAuthKey {
		t.Fatalf("missing key: got %v", err)
	}
	os.WriteFile(path, []byte(`{"auth-key": " from-file "}`), 0o644)
	store, err := Load(path, dir)
	if err != nil || store.AuthKey() != "from-file" {
		t.Fatalf("file key: %v %q", err, store.AuthKey())
	}
	t.Setenv("CHATGPT2API_AUTH_KEY", "from-env")
	if store.AuthKey() != "from-env" {
		t.Fatalf("env key: %q", store.AuthKey())
	}
	os.WriteFile(path, []byte(`not json`), 0o644)
	if store, err := Load(path, dir); err != nil || len(store.Get()) == 0 {
		t.Fatalf("invalid json must load as an empty object: %v", err)
	}
}
