package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestItemReserve(t *testing.T) {
	item := Item{SKU: "A", OnHand: 10, Reserved: 4}

	tests := []struct {
		name         string
		qty          int
		wantReserved int
		wantErr      error
	}{
		{"within availability", 6, 10, nil},
		{"one", 1, 5, nil},
		{"zero", 0, 4, ErrInvalidQuantity},
		{"negative", -1, 4, ErrInvalidQuantity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := item.Reserve(tc.qty)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got.Reserved != tc.wantReserved {
				t.Errorf("Reserved = %d, want %d", got.Reserved, tc.wantReserved)
			}
		})
	}

	t.Run("more than available", func(t *testing.T) {
		_, err := item.Reserve(7)
		var short *InsufficientStockError
		if !errors.As(err, &short) || !reflect.DeepEqual(short.SKUs, []string{"A"}) {
			t.Fatalf("err = %v, want InsufficientStockError for A", err)
		}
	})

	t.Run("does not mutate the receiver", func(t *testing.T) {
		_, _ = item.Reserve(2)
		if item.Reserved != 4 {
			t.Fatalf("receiver changed: %+v", item)
		}
	})
}

func TestItemAvailable(t *testing.T) {
	if got := (Item{OnHand: 10, Reserved: 3}).Available(); got != 7 {
		t.Fatalf("Available = %d, want 7", got)
	}
}

func TestItemRelease(t *testing.T) {
	item := Item{SKU: "A", OnHand: 10, Reserved: 4}

	got, err := item.Release(3)
	if err != nil || got.Reserved != 1 {
		t.Fatalf("Release(3) = %+v, %v", got, err)
	}
	if _, err := item.Release(5); err == nil {
		t.Error("releasing more than reserved must fail")
	}
	if _, err := item.Release(0); !errors.Is(err, ErrInvalidQuantity) {
		t.Errorf("Release(0) err = %v", err)
	}
}

func TestNewReservationNormalisesLines(t *testing.T) {
	r, err := NewReservation("o-1", []Line{{"B", 1}, {"A", 2}, {"B", 3}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{"A", 2}, {"B", 4}}
	if !reflect.DeepEqual(r.Lines, want) {
		t.Fatalf("Lines = %v, want %v (merged and sorted)", r.Lines, want)
	}
	if r.Status != ReservationActive {
		t.Errorf("Status = %q", r.Status)
	}
	if got := r.SKUs(); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("SKUs = %v", got)
	}
}

func TestNewReservationValidation(t *testing.T) {
	tests := []struct {
		name    string
		orderID string
		lines   []Line
	}{
		{"no order id", "", []Line{{"A", 1}}},
		{"no lines", "o-1", nil},
		{"empty sku", "o-1", []Line{{"", 1}}},
		{"zero quantity", "o-1", []Line{{"A", 0}}},
		{"negative quantity", "o-1", []Line{{"A", -2}}},
		{"quantity above the limit", "o-1", []Line{{"A", MaxQuantity + 1}}},
		{"merged quantity above the limit", "o-1", []Line{{"A", MaxQuantity}, {"A", 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReservation(tc.orderID, tc.lines); !errors.Is(err, ErrInvalidReservation) {
				t.Fatalf("err = %v, want ErrInvalidReservation", err)
			}
		})
	}
}

func TestReservationReserveIsAllOrNothing(t *testing.T) {
	items := map[string]Item{
		"A": {SKU: "A", OnHand: 5},
		"B": {SKU: "B", OnHand: 1},
	}
	r, _ := NewReservation("o-1", []Line{{"A", 2}, {"B", 2}, {"C", 1}})

	got, err := r.Reserve(items)
	var short *InsufficientStockError
	if !errors.As(err, &short) {
		t.Fatalf("err = %v, want InsufficientStockError", err)
	}
	if !reflect.DeepEqual(short.SKUs, []string{"B", "C"}) {
		t.Errorf("short SKUs = %v, want [B C] (short and unknown)", short.SKUs)
	}
	if got != nil {
		t.Errorf("no items should be returned on failure, got %v", got)
	}
	if items["A"].Reserved != 0 {
		t.Error("input items must not be modified")
	}
}

func TestReservationReserveAndRelease(t *testing.T) {
	items := map[string]Item{
		"A": {SKU: "A", OnHand: 5},
		"B": {SKU: "B", OnHand: 5},
	}
	r, _ := NewReservation("o-1", []Line{{"A", 2}, {"B", 3}})

	reserved, err := r.Reserve(items)
	if err != nil {
		t.Fatal(err)
	}
	if reserved["A"].Reserved != 2 || reserved["B"].Reserved != 3 {
		t.Fatalf("reserved = %+v", reserved)
	}

	released, err := r.Release(reserved)
	if err != nil {
		t.Fatal(err)
	}
	if released["A"].Reserved != 0 || released["B"].Reserved != 0 {
		t.Fatalf("released = %+v", released)
	}
}

func TestReservationReleaseUnknownItem(t *testing.T) {
	r, _ := NewReservation("o-1", []Line{{"A", 1}})
	if _, err := r.Release(map[string]Item{}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}
