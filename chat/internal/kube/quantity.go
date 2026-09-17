package kube

import (
	"errors"
	"math"
	"math/big"
	"strings"
)

// ParseQuantity reads a Kubernetes resource quantity ("109m", "1086Mi",
// "2", "1.5Gi", "3e3", "500M") into a float in base units: cores for CPU,
// bytes for memory. Binary (Ki, Mi, ...) and decimal (k, M, ...) suffixes
// and the milli suffix are accepted; anything else is an error. The
// result is what a page displays, so a float is enough.
func ParseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty quantity")
	}
	scale := 1.0
	suffixes := []struct {
		suffix string
		scale  float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60},
		{"m", 1e-3}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18},
	}
	number := s
	for _, sfx := range suffixes {
		if strings.HasSuffix(s, sfx.suffix) {
			number, scale = strings.TrimSuffix(s, sfx.suffix), sfx.scale
			break
		}
	}
	// big.Float takes the decimal and exponent forms ("1.5", "3e3", "12E2")
	// without reading a trailing E as a suffix, which the loop above already
	// stripped.
	f, ok := new(big.Float).SetString(number)
	if !ok || strings.ContainsAny(number, " +") && !strings.HasPrefix(number, "+") {
		return 0, errors.New("invalid quantity " + s)
	}
	v, _ := f.Float64()
	if math.IsInf(v, 0) || math.IsNaN(v) || v < 0 {
		return 0, errors.New("invalid quantity " + s)
	}
	return v * scale, nil
}

// Milli returns a quantity in thousandths, rounded, as CPU is reported.
func Milli(s string) (int64, error) {
	v, err := ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(v * 1000)), nil
}

// Bytes returns a quantity in whole base units, rounded, as memory is
// reported.
func Bytes(s string) (int64, error) {
	v, err := ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(v)), nil
}
