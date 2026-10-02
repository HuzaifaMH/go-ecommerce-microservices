package pagination

import (
	"errors"
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 123456789, time.UTC)

	got, id, ok, err := Decode(Encode(at, "order-42"))
	if err != nil || !ok || id != "order-42" || !got.Equal(at) {
		t.Fatalf("got %v %q ok=%v err=%v", got, id, ok, err)
	}
}

func TestIDsContainingTheSeparatorSurvive(t *testing.T) {
	_, id, ok, err := Decode(Encode(time.Unix(1, 0), "a|b|c"))
	if err != nil || !ok || id != "a|b|c" {
		t.Fatalf("id = %q ok=%v err=%v", id, ok, err)
	}
}

func TestEmptyTokenMeansFirstPage(t *testing.T) {
	_, _, ok, err := Decode("")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v; want first page", ok, err)
	}
}

func TestInvalidTokens(t *testing.T) {
	tests := map[string]string{
		"not base64":    "!!!not-base64!!!",
		"no separator":  "bm9waXBl", // "nopipe"
		"empty id":      "MTIzfA",   // "123|"
		"bad timestamp": "eHl6fGlk", // "xyz|id"
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := Decode(token); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestSize(t *testing.T) {
	tests := []struct {
		in      int32
		want    int
		wantErr bool
	}{
		{0, DefaultSize, false},
		{1, 1, false},
		{MaxSize, MaxSize, false},
		{MaxSize + 1, MaxSize, false},
		{1 << 30, MaxSize, false},
		{-1, 0, true},
	}
	for _, tc := range tests {
		got, err := Size(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Size(%d) = %d, %v; want %d (err %v)", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}
