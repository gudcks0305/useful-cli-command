package text

import "testing"

func TestTruncateHandlesUnicodeAndSmallLimits(t *testing.T) {
	tests := []struct {
		input string
		limit int
		want  string
	}{
		{"가나다라마바사", 5, "가나..."},
		{"abcdef", 3, "..."},
		{"abcdef", 2, ".."},
		{"abcdef", 0, ""},
	}
	for _, test := range tests {
		if got := Truncate(test.input, test.limit); got != test.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", test.input, test.limit, got, test.want)
		}
	}
}

func TestTruncatePathUsesHomeBoundaryAndUnicode(t *testing.T) {
	if got := TruncatePath("/home/user2/file", 100, "/home/user"); got != "/home/user2/file" {
		t.Fatalf("prefix collision: %q", got)
	}
	if got := TruncatePath("/home/user/가나다라마바사", 8, "/home/user"); got != "...다라마바사" {
		t.Fatalf("unicode path = %q", got)
	}
}

func TestParseSizeE(t *testing.T) {
	tests := map[string]int64{
		"1B":      1,
		"1kb":     1 << 10,
		"1KiB":    1 << 10,
		"1MB":     1 << 20,
		"1mib":    1 << 20,
		"1GB":     1 << 30,
		"1gIb":    1 << 30,
		"1TB":     1 << 40,
		"1TiB":    1 << 40,
		"1.5 MB":  1572864,
		"0.5KiB":  512,
		"1.9B":    1,
		"1024":    1024,
		"0":       0,
		"":        0,
		"8.5e1 B": 85,
	}
	for input, want := range tests {
		got, err := ParseSizeE(input)
		if err != nil || got != want {
			t.Errorf("ParseSizeE(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	max, err := ParseSizeE("9223372036854775807B")
	if err != nil || max != int64(^uint64(0)>>1) {
		t.Fatalf("max int64 = %d, %v", max, err)
	}
	for _, input := range []string{"wat", "1PB", "-1MB", "NaN", "Inf", "1/2B", "0x10B", "9223372036854775808B", "999999999999999999999GB"} {
		if _, err := ParseSizeE(input); err == nil {
			t.Errorf("ParseSizeE(%q) succeeded", input)
		}
	}
}

func TestSeparatorNegativeIsEmpty(t *testing.T) {
	if got := Separator(-1); got != "" {
		t.Fatalf("Separator(-1) = %q", got)
	}
	if got := SeparatorAuto(-1); got != "" {
		t.Fatalf("SeparatorAuto(-1) = %q", got)
	}
}

func TestNaturalLess(t *testing.T) {
	if !NaturalLess("file2", "file10") || NaturalLess("file10", "file2") {
		t.Fatal("natural numeric order broken")
	}
}
