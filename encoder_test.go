package eventsource

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type encoderTestCase struct {
	event    publication
	expected string
}

type streamingSplitterTestCase struct {
	payload         string
	bufferSize      int
	expectedResults []string
}

type writerWithOnlyWriteMethod struct {
	buf *bytes.Buffer
}

func (w *writerWithOnlyWriteMethod) Write(data []byte) (int, error) {
	return w.buf.Write(data)
}

func TestStreamingNewlineSplitter(t *testing.T) {
	for _, tc := range []streamingSplitterTestCase{
		{payload: "Hi there", bufferSize: 1_000, expectedResults: []string{"Hi there"}},
		{payload: "Hi\nI have newlines\n", bufferSize: 1_000, expectedResults: []string{"Hi\n", "I have newlines\n", ""}},
		{payload: "\n\n\n", bufferSize: 1_000, expectedResults: []string{"\n", "\n", "\n", ""}},
		{payload: "Double new line, what does it mean?\n\n", bufferSize: 1_000, expectedResults: []string{"Double new line, what does it mean?\n", "\n", ""}},
	} {
		scanner := bufio.NewScanner(strings.NewReader(tc.payload))
		scanner.Split(newStreamingNewlineSplitter(tc.bufferSize).scan)

		results := make([]string, 0)
		for scanner.Scan() {
			results = append(results, scanner.Text())
		}

		assert.Equal(t, tc.expectedResults, results)
	}
}

func TestEncoderOmitsOptionalFieldsWithoutValues(t *testing.T) {
	for _, tc := range []encoderTestCase{
		{*newPublicationEvent("", "", "", "aaa"), "data: aaa\n\n"},
		{*newPublicationEvent("", "aaa", "", "bbb"), "event: aaa\ndata: bbb\n\n"},
		{*newPublicationEvent("aaa", "", "", "bbb"), "id: aaa\ndata: bbb\n\n"},
		{*newPublicationEvent("aaa", "bbb", "", "ccc"), "id: aaa\nevent: bbb\ndata: ccc\n\n"},

		// An SSE message must *always* have a data field, even if its value is empty.
		{*newPublicationEvent("", "", "", ""), "data: \n\n"},
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
		{*newPublicationEvent("", "", "", "\nfirst"), "data: \ndata: first\n\n"},
		{*newPublicationEvent("", "", "", "first\nsecond"), "data: first\ndata: second\n\n"},
		{*newPublicationEvent("", "", "", "first\nsecond\nthird"), "data: first\ndata: second\ndata: third\n\n"},
		{*newPublicationEvent("", "", "", "ends with newline\n"), "data: ends with newline\ndata: \n\n"},
		{*newPublicationEvent("", "", "", "first\nends with newline\n"), "data: first\ndata: ends with newline\ndata: \n\n"},
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
	c := comment{value: "hello"}
	NewEncoder(buf, false).Encode(c)
	assert.Equal(t, ":hello\n", string(buf.Bytes()))
}

func TestEncoderGzipCompression(t *testing.T) {
	uncompressedBuf, compressedBuf, expectedCompressedBuf := bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	event := newPublicationEvent("", "aaa", "", "bbb")

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
		enc.Encode(newPublicationEvent("aaa", "bbb", "", "ccc"))
		if withWriteString {
			w.(*bufio.Writer).Flush()
		}
		assert.Equal(t, "id: aaa\nevent: bbb\ndata: ccc\n\n", string(buf.Bytes()))
	}

	t.Run("with WriteString", func(t *testing.T) { doTest(t, true) })
	t.Run("without WriteString", func(t *testing.T) { doTest(t, false) })
}
