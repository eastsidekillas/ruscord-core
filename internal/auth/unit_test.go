package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(1)
	hash, err := h.Hash(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=2,p=2$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if ok, err := h.Verify(t.Context(), "correct horse", hash); err != nil || !ok {
		t.Fatalf("Verify(correct) = %v, %v", ok, err)
	}
	if ok, _ := h.Verify(t.Context(), "wrong horse", hash); ok {
		t.Fatal("Verify(wrong) = true")
	}
	other, _ := h.Hash(t.Context(), "correct horse")
	if other == hash {
		t.Fatal("hashes of the same password must differ (salt)")
	}
	if _, err := h.Verify(t.Context(), "x", "$bcrypt$nope"); err == nil {
		t.Fatal("expected error for malformed hash")
	}
}

func TestAccessToken(t *testing.T) {
	seed := make([]byte, 32)
	s, err := NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{UserID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7())}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tok, err := s.Issue(p, now)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Verify(tok, now.Add(5*time.Minute))
	if err != nil || got != p {
		t.Fatalf("Verify = %v, %v; want %v", got, err, p)
	}
	if _, err := s.Verify(tok, now.Add(accessTTL+time.Minute)); err == nil {
		t.Fatal("expired token accepted")
	}

	otherSeed := make([]byte, 32)
	otherSeed[0] = 1
	other, _ := NewSigner(otherSeed)
	if _, err := other.Verify(tok, now); err == nil {
		t.Fatal("token signed by another key accepted")
	}

	// alg=none and HS256 forgeries must be rejected.
	parts := strings.Split(tok, ".")
	for _, header := range []string{"eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"} {
		if _, err := s.Verify(header+"."+parts[1]+".", now); err == nil {
			t.Fatalf("forged token with header %s accepted", header)
		}
	}
}

func TestRefreshEncoding(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	secret, _ := newRefreshSecret()
	gotID, gotSecret, err := decodeRefresh(encodeRefresh(id, secret))
	if err != nil || gotID != id || string(gotSecret) != string(secret) {
		t.Fatalf("round trip failed: %v %v %v", gotID, gotSecret, err)
	}
	for _, bad := range []string{"", "abc", "!!!!", strings.Repeat("A", 70)} {
		if _, _, err := decodeRefresh(bad); err == nil {
			t.Errorf("decodeRefresh(%q) succeeded", bad)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	tests := []struct {
		name  string
		in    RegisterInput
		field string
	}{
		{"ok", RegisterInput{Email: "a@b.io", Username: "timofey", Password: "longenough"}, ""},
		{"bad email", RegisterInput{Email: "nope", Username: "timofey", Password: "longenough"}, "email"},
		{"email with name", RegisterInput{Email: "Tim <a@b.io>", Username: "timofey", Password: "longenough"}, "email"},
		{"short username", RegisterInput{Email: "a@b.io", Username: "t", Password: "longenough"}, "username"},
		{"cyrillic username", RegisterInput{Email: "a@b.io", Username: "тимофей", Password: "longenough"}, "username"},
		{"double dot", RegisterInput{Email: "a@b.io", Username: "a..b", Password: "longenough"}, "username"},
		{"short password", RegisterInput{Email: "a@b.io", Username: "timofey", Password: "short"}, "password"},
		{"password is username", RegisterInput{Email: "a@b.io", Username: "timofey123", Password: "Timofey123"}, "password"},
		{"long display name", RegisterInput{Email: "a@b.io", Username: "timofey", DisplayName: strings.Repeat("я", 33), Password: "longenough"}, "display_name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			err := in.normalize()
			if tt.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err.Fields)
				}
				if in.DisplayName != in.Username {
					t.Errorf("display name defaults to username, got %q", in.DisplayName)
				}
				return
			}
			if err == nil || err.Fields[tt.field] == "" {
				t.Fatalf("want error on %s, got %v", tt.field, err)
			}
		})
	}
}
