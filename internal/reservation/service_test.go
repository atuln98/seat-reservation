package reservation

import (
	"testing"
)

func TestSameRequest(t *testing.T) {
	existing := Reservation{
		ShowID: "1a6d7e70-2220-4b39-a1d0-5fb24760fb43",
		Seats:  []string{"A1", "A2"},
	}

	tests := []struct {
		name   string
		showID string
		seats  []string
		want   bool
	}{
		{
			name:   "same show and seats",
			showID: existing.ShowID,
			seats:  []string{"A1", "A2"},
			want:   true,
		},
		{
			name:   "different show",
			showID: "0d7b5331-cd07-4686-a77a-899243647162",
			seats:  []string{"A1", "A2"},
			want:   false,
		},
		{
			name:   "different seats",
			showID: existing.ShowID,
			seats:  []string{"A1", "A3"},
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sameRequest(existing, test.showID, test.seats); got != test.want {
				t.Fatalf("sameRequest() = %t, want %t", got, test.want)
			}
		})
	}
}
