package openai

import (
	"bytes"
	"io"
	"sync"

	"github.com/valyala/bytebufferpool"
)

// pooledBody is a request body in a pooled buffer. net/http may still read
// a request body after the response has arrived, and says to wait for the
// body's Close before reusing it, so release waits for every reader handed
// out.
type pooledBody struct {
	buf     *bytebufferpool.ByteBuffer
	readers sync.WaitGroup
}

func newPooledBody() *pooledBody { return &pooledBody{buf: bytebufferpool.Get()} }

func (b *pooledBody) reader() io.ReadCloser {
	b.readers.Add(1)
	return &pooledReader{Reader: bytes.NewReader(b.buf.B), body: b}
}

// release returns the buffer once the transport has closed every reader.
// It is called after the response body is closed.
func (b *pooledBody) release() {
	b.readers.Wait()
	bytebufferpool.Put(b.buf)
}

type pooledReader struct {
	*bytes.Reader
	body   *pooledBody
	closed sync.Once
}

func (r *pooledReader) Close() error {
	r.closed.Do(r.body.readers.Done)
	return nil
}
