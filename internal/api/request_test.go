package api

import "testing"

func TestCanonicalUUID(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{
			name:  "canonical",
			value: "1a6d7e70-2220-4b39-a1d0-5fb24760fb43",
			want:  "1a6d7e70-2220-4b39-a1d0-5fb24760fb43",
			ok:    true,
		},
		{
			name:  "uppercase",
			value: "1A6D7E70-2220-4B39-A1D0-5FB24760FB43",
			want:  "1a6d7e70-2220-4b39-a1d0-5fb24760fb43",
			ok:    true,
		},
		{
			name:  "without separators",
			value: "1a6d7e7022204b39a1d05fb24760fb43",
			want:  "1a6d7e70-2220-4b39-a1d0-5fb24760fb43",
			ok:    true,
		},
		{
			name:  "invalid",
			value: "not-a-uuid",
			ok:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := canonicalUUID(test.value)
			if ok != test.ok || got != test.want {
				t.Fatalf("canonicalUUID() = %q, %t, want %q, %t", got, ok, test.want, test.ok)
			}
		})
	}
}
