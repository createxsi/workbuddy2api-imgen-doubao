package main

import "testing"

func TestInsecureKeyWarning(t *testing.T) {
	cases := []struct {
		listen, key string
		warn        bool
	}{
		{"127.0.0.1:7863", "", false},
		{"[::1]:7863", "test_key", false},
		{"localhost:7863", "", false},
		{":7863", "", true},
		{"0.0.0.0:7863", "test_key", true},
		{"0.0.0.0:7863", "sk-random", false},
		{"192.168.1.2:7863", "", true},
	}
	for _, c := range cases {
		if got := insecureKeyWarning(c.listen, c.key) != ""; got != c.warn {
			t.Errorf("listen=%q key=%q warn=%v want %v", c.listen, c.key, got, c.warn)
		}
	}
}
