package eventsource

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type testEvent struct {
	id, event, data string
}

func newTestEvent(id, event, data string) *testEvent {
	return &testEvent{
		id:    id,
		event: event,
		data:  data,
	}
}

func (e *testEvent) Id() string           { return e.id }
func (e *testEvent) Event() string        { return e.event }
func (e *testEvent) GetReader() io.Reader { return strings.NewReader(e.data) }

var encoderTests = []struct {
	event  *testEvent
	output string
}{
	{newTestEvent("1", "Add", "This is a test"), "id: 1\nevent: Add\ndata: This is a test\n\n"},
	{newTestEvent("", "", "This message, it\nhas two lines."), "data: This message, it\ndata: has two lines.\n\n"},
}

func TestRoundTrip(t *testing.T) {
	for _, tt := range encoderTests {
		buf := new(bytes.Buffer)
		enc := NewEncoder(buf, false)
		want := tt.event
		if err := enc.Encode(want); err != nil {
			t.Fatal(err)
		}
		if buf.String() != tt.output {
			t.Errorf("Expected: %s Got: %s", tt.output, buf.String())
		}
		dec := NewDecoder(buf)
		ev, err := dec.Decode()
		if err != nil {
			t.Fatal(err)
		}

		evData, err := io.ReadAll(ev.GetReader())
		assert.NoError(t, err)

		wantData, err := io.ReadAll(want.GetReader())
		assert.NoError(t, err)

		if ev.Id() != want.Id() || ev.Event() != want.Event() || !bytes.Equal(evData, wantData) {
			t.Errorf("Expected: '%s' '%s' '%s' Got: '%s' '%s' '%s'", want.Id(), want.Event(), string(wantData), ev.Id(), ev.Event(), string(evData))
		}
	}
}

func TestEncodeComment(t *testing.T) {
	buf := new(bytes.Buffer)
	enc := NewEncoder(buf, false)
	text := "This is a comment"
	comm := comment{value: "This is a comment"}
	expected := ":" + text + "\n"
	if err := enc.Encode(comm); err != nil {
		t.Fatal(err)
	}
	if buf.String() != expected {
		t.Errorf("Expected: %s Got: %s", expected, buf.String())
	}
}

func TestEncodeUnknownValue(t *testing.T) {
	buf := new(bytes.Buffer)
	enc := NewEncoder(buf, false)
	badValue := 3
	if err := enc.Encode(badValue); err == nil {
		t.Error("Expected error")
	}
}
