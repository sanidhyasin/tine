package main

import "testing"

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"4G": 4 << 30, "4g": 4 << 30, "4GB": 4 << 30, "4GiB": 4 << 30,
		"512M": 512 << 20, "1.5G": 3 << 29, "100K": 100 << 10, "1T": 1 << 40,
		"1048576": 1 << 20, " 2G ": 2 << 30,
	} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "abc", "-1G", "0", "4X"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q): want an error", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{4 << 30: "4 GiB", 1536 << 20: "1.5 GiB", 512 << 20: "512 MiB", 100: "100 bytes"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}
