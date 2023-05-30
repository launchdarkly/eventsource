package eventsource

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
)

var (
	encFields = []struct { //nolint:gochecknoglobals // non-exported global that we treat as a constant
		prefix   string
		value    func(Event) string
		required bool
	}{
		{"id: ", Event.Id, false},
		{"event: ", Event.Event, false},
	}
)

// An Encoder is capable of writing Events to a stream. Optionally
// Events can be gzip compressed in this process.
type Encoder struct {
	w          io.Writer
	compressed bool
}

// NewEncoder returns an Encoder for a given io.Writer.
// When compressed is set to true, a gzip writer will be
// created.
func NewEncoder(w io.Writer, compressed bool) *Encoder {
	if compressed {
		return &Encoder{w: gzip.NewWriter(w), compressed: true}
	}
	return &Encoder{w: w}
}

type streamingNewlineSplitter struct {
	bufferSize              int
	hasProcessedData        bool
	lastEventEndedInNewline bool
}

func newStreamingNewlineSplitter(bufferSize int) *streamingNewlineSplitter {
	return &streamingNewlineSplitter{bufferSize: bufferSize}

}

func (s *streamingNewlineSplitter) scan(data []byte, atEOF bool) (advance int, token []byte, err error) {
	// We are at the end of the data stream, and we have no remaining data to process.
	if atEOF && len(data) == 0 {
		// If the last event ended with a newline, then we need to return one
		// final empty segment.
		//
		// Alternatively, if we have NEVER processed anything, then we should
		// ensure we send at least one empty segment.
		if s.lastEventEndedInNewline || !s.hasProcessedData {
			s.lastEventEndedInNewline = false
			s.hasProcessedData = true
			return 0, []byte(""), nil
		}

		return 0, nil, nil
	}

	// If the current block has a newline, then we can return that as a block.
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		s.hasProcessedData = true
		s.lastEventEndedInNewline = true
		return i + 1, data[0 : i+1], nil
	}

	// There is no guarantee that the data will every contain a new line. So we
	// set a limit on the number of bytes we can process before we return a
	// payload.
	if len(data) >= s.bufferSize {
		s.hasProcessedData = true
		return len(data), data, nil
	}

	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		s.hasProcessedData = true
		return len(data), data, nil
	}

	// Request more data.
	return 0, nil, nil
}

type sseStreamingWriter struct {
	writer     io.Writer
	bufferSize int
}

func (s *sseStreamingWriter) Write(p []byte) (n int, err error) {
	reader := bytes.NewReader(p)
	scanner := bufio.NewScanner(reader)
	scanner.Split(newStreamingNewlineSplitter(s.bufferSize).scan)

	startNewEvent := true
	processed := 0

	for scanner.Scan() {
		// This scan text may include a newline if it was split on that
		// boundary, or it might not, if it was returned due to buffer
		// limit constraints.
		//
		// If there isn't a newline, then we can continue streaming it as
		// part of the current event. Otherwise, we need to start a new
		// event.
		text := scanner.Text()
		if startNewEvent {
			if _, err := io.WriteString(s.writer, "data: "); err != nil {
				return processed, fmt.Errorf("eventsource encode: %v", err)
			}
		}

		bytesWritten, err := io.WriteString(s.writer, text)
		if err != nil {
			return processed, fmt.Errorf("eventsource encode: %v", err)
		}

		processed += bytesWritten

		startNewEvent = strings.HasSuffix(text, "\n")
	}

	// If the final block didn't end in a newline, we need to ensure we write a final one.
	if !startNewEvent {
		if _, err := io.WriteString(s.writer, "\n"); err != nil {
			return processed, fmt.Errorf("eventsource encode: %v", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return processed, fmt.Errorf("eventsource encode: %v", err)
	}

	return processed, nil
}

// Encode writes an event or comment in the format specified by the
// server-sent events protocol.
func (enc *Encoder) Encode(ec eventOrComment) error {
	switch item := ec.(type) {
	case Event:
		for _, field := range encFields {
			prefix, value := field.prefix, field.value(item)
			if len(value) == 0 && !field.required {
				continue
			}

			for _, s := range strings.Split(value, "\n") {
				if _, err := io.WriteString(enc.w, prefix); err != nil {
					return fmt.Errorf("eventsource encode: %v", err)
				}
				if _, err := io.WriteString(enc.w, s); err != nil {
					return fmt.Errorf("eventsource encode: %v", err)
				}
				if _, err := io.WriteString(enc.w, "\n"); err != nil {
					return fmt.Errorf("eventsource encode: %v", err)
				}
			}
		}

		written, err := io.Copy(&sseStreamingWriter{writer: enc.w, bufferSize: 1_000}, item.GetReader())
		if err != nil {
			return err
		}

		if written == 0 {
			if _, err := io.WriteString(enc.w, "data: \n"); err != nil {
				return fmt.Errorf("eventsource encode: %v", err)
			}
		}

		// Every payload ends with a newline.
		if _, err := io.WriteString(enc.w, "\n"); err != nil {
			return fmt.Errorf("eventsource encode: %v", err)
		}
	case comment:
		line := ":" + item.value + "\n"
		if _, err := io.WriteString(enc.w, line); err != nil {
			return fmt.Errorf("eventsource encode: %v", err)
		}
	default:
		return fmt.Errorf("unexpected parameter to Encode: %v", ec)
	}
	if enc.compressed {
		return enc.w.(*gzip.Writer).Flush()
	}
	return nil
}
