package imagegen

import (
	"encoding/json"
	"os"
	"testing"
)

// TestTokenCountsMatchPython replays tools/vectors/token_vectors.py.
func TestTokenCountsMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/token_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Text []struct {
			Text, Model string
			Tokens      int
		}
		Input []struct {
			W, H   int
			Detail string
			Tokens int
		}
		Generated []struct {
			W, H    int
			Quality string
			Tokens  int
		}
		Parse []struct {
			Size *string
			WH   []int
		}
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Text {
		if got := CountTextTokens(c.Text, c.Model); got != c.Tokens {
			t.Errorf("text %q %s: got %d want %d", c.Text, c.Model, got, c.Tokens)
		}
	}
	for _, c := range v.Input {
		if got := CountImageInputTokens(c.W, c.H, c.Detail); got != c.Tokens {
			t.Errorf("input %dx%d %s: got %d want %d", c.W, c.H, c.Detail, got, c.Tokens)
		}
	}
	for _, c := range v.Generated {
		if got := CountGeneratedImageTokens(c.W, c.H, c.Quality); got != c.Tokens {
			t.Errorf("generated %dx%d %s: got %d want %d", c.W, c.H, c.Quality, got, c.Tokens)
		}
	}
	for _, c := range v.Parse {
		size := ""
		if c.Size != nil {
			size = *c.Size
		}
		if w, h := ParseImageSize(size); w != c.WH[0] || h != c.WH[1] {
			t.Errorf("parse %q: got %dx%d want %v", size, w, h, c.WH)
		}
	}
	t.Logf("%d text, %d input, %d generated, %d parse vectors", len(v.Text), len(v.Input), len(v.Generated), len(v.Parse))
}
