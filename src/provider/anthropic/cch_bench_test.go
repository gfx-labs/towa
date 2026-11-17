package anthropic

import (
	"strings"
	"testing"
)

// BenchmarkSignBilling signs a 512 KB body, the size of a long session's
// request.
func BenchmarkSignBilling(b *testing.B) {
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":64000,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.325; cch=00000;"}],"messages":[{"role":"user","content":"` +
		strings.Repeat("a long conversation ", 512*1024/20) + `"}]}`)
	work := make([]byte, len(body))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		copy(work, body)
		signBilling(work)
	}
}
