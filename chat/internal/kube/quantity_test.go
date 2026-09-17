package kube

import "testing"

func TestParseQuantity(t *testing.T) {
	cases := map[string]float64{
		"109m": 0.109, "2": 2, "1.5": 1.5, "1086Mi": 1086 << 20, "1Gi": 1 << 30, "500M": 5e8, "3e3": 3000, "12E2": 1200, "0": 0, "8138104Ki": 8138104 << 10,
	}
	for in, want := range cases {
		got, err := ParseQuantity(in)
		if err != nil || got != want {
			t.Errorf("ParseQuantity(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1 2", "-1", "Mi", "1.5.2"} {
		if _, err := ParseQuantity(bad); err == nil {
			t.Errorf("ParseQuantity(%q) accepted", bad)
		}
	}
	if m, _ := Milli("109m"); m != 109 {
		t.Errorf("Milli = %d", m)
	}
	if m, _ := Milli("2"); m != 2000 {
		t.Errorf("Milli(2) = %d", m)
	}
	if b, _ := Bytes("1086Mi"); b != 1086<<20 {
		t.Errorf("Bytes = %d", b)
	}
}
