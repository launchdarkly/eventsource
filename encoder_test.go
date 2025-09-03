package eventsource

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

type encoderTestCase struct {
	event    publication
	expected string
}

type writerWithOnlyWriteMethod struct {
	buf *bytes.Buffer
}

func (w *writerWithOnlyWriteMethod) Write(data []byte) (int, error) {
	return w.buf.Write(data)
}

func TestEncoderOmitsOptionalFieldsWithoutValues(t *testing.T) {
	for _, tc := range []encoderTestCase{
		{publication{data: "aaa"}, "data: aaa\n\n"},
		{publication{event: "aaa", data: "bbb"}, "event: aaa\ndata: bbb\n\n"},
		{publication{id: "aaa", data: "bbb"}, "id: aaa\ndata: bbb\n\n"},
		{publication{id: "aaa", event: "bbb", data: "ccc"}, "id: aaa\nevent: bbb\ndata: ccc\n\n"},

		// An SSE message must *always* have a data field, even if its value is empty.
		{publication{data: ""}, "data: \n\n"},
	} {
		t.Run(fmt.Sprintf("%+v", tc.event), func(t *testing.T) {
			buf := bytes.NewBuffer(nil)
			NewEncoder(buf, false).Encode(&tc.event)
			assert.Equal(t, tc.expected, string(buf.Bytes()))
		})
	}
}

func TestEncoderMultiLineData(t *testing.T) {
	for _, tc := range []encoderTestCase{
		{publication{data: "\nfirst"}, "data: \ndata: first\n\n"},
		{publication{data: "first\nsecond"}, "data: first\ndata: second\n\n"},
		{publication{data: "first\nsecond\nthird"}, "data: first\ndata: second\ndata: third\n\n"},
		{publication{data: "ends with newline\n"}, "data: ends with newline\ndata: \n\n"},
		{publication{data: "first\nends with newline\n"}, "data: first\ndata: ends with newline\ndata: \n\n"},
	} {
		t.Run(fmt.Sprintf("%+v", tc.event), func(t *testing.T) {
			buf := bytes.NewBuffer(nil)
			NewEncoder(buf, false).Encode(&tc.event)
			assert.Equal(t, tc.expected, string(buf.Bytes()))
		})
	}
}

func TestEncoderComment(t *testing.T) {
	buf := bytes.NewBuffer(nil)
	c := Comment("hello")
	NewEncoder(buf, false).Encode(c)
	assert.Equal(t, ":hello\n", string(buf.Bytes()))
}

func TestEncoderGzipCompression(t *testing.T) {
	uncompressedBuf, compressedBuf, expectedCompressedBuf := bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	event := &publication{
		event: "aaa",
		data:  "bbb",
	}

	NewEncoder(uncompressedBuf, false).Encode(event)
	zipper := gzip.NewWriter(expectedCompressedBuf)
	zipper.Write(uncompressedBuf.Bytes())
	zipper.Flush()

	NewEncoder(compressedBuf, true).Encode(event)
	assert.Equal(t, expectedCompressedBuf.Bytes(), compressedBuf.Bytes())
}

func TestEncoderCanWriteToWriterWithOrWithoutWriteStringMethod(t *testing.T) {
	// We're using bufio.WriteString, so that we should get consistent output regardless of whether
	// the underlying Writer supports the WriteString method (as the standard http.ResponseWriter
	// does), but that is an implementation detail so this test verifies that it really does work.
	doTest := func(t *testing.T, withWriteString bool) {
		var w io.Writer
		buf := bytes.NewBuffer(nil)
		if withWriteString {
			w = bufio.NewWriter(buf)
		} else {
			w = &writerWithOnlyWriteMethod{buf: buf}
		}
		enc := NewEncoder(w, false)
		enc.Encode(&publication{
			id:    "aaa",
			event: "bbb",
			data:  "ccc",
		})
		if withWriteString {
			w.(*bufio.Writer).Flush()
		}
		assert.Equal(t, "id: aaa\nevent: bbb\ndata: ccc\n\n", string(buf.Bytes()))
	}

	t.Run("with WriteString", func(t *testing.T) { doTest(t, true) })
	t.Run("without WriteString", func(t *testing.T) { doTest(t, false) })
}
