package httpx

import (
	"encoding/json"
	"testing"
)

func TestFieldsLength(t *testing.T) {
	for _, c := range []struct {
		name, v string
		ok      bool
	}{
		{"min", "ab", true},
		{"max in runes, not bytes", "яяяя", true},
		{"too short", "a", false},
		{"too long", "abcde", false},
		{"NUL inside", "a\x00b", false},
	} {
		f := Fields{}
		f.Length("x", c.v, 2, 4)
		if (f.Err() == nil) != c.ok {
			t.Errorf("%s: %q gives %v", c.name, c.v, f)
		}
	}
}

func TestOptional(t *testing.T) {
	var in struct {
		A Optional[string] `json:"a"`
		B Optional[string] `json:"b"`
		C Optional[string] `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a": "x", "b": null}`), &in); err != nil {
		t.Fatal(err)
	}
	if !in.A.Set || *in.A.Value != "x" || !in.B.Set || in.B.Value != nil || in.C.Set {
		t.Fatalf("got %+v", in)
	}
}
