package shell

import (
	"context"
	"strings"
	"testing"
)

// BenchmarkDrain reads the output of a typical command, a few hundred bytes,
// as every bash call does.
func BenchmarkDrain(b *testing.B) {
	output := strings.Repeat("a line of output\n", 20)
	b.ReportAllocs()
	for b.Loop() {
		c := &collector{limit: 1 << 20}
		if err := c.drain(context.Background(), strings.NewReader(output)); err != nil {
			b.Fatal(err)
		}
	}
}
