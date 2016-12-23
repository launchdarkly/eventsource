package eventsource

import (
	"bytes"
	"testing"
	"time"
)

type testEvent struct {
	id, event, data string
}

func (e *testEvent) Id() string    { return e.id }
func (e *testEvent) Event() string { return e.event }
func (e *testEvent) Data() string  { return e.data }

var encoderTests = []struct {
	event  *testEvent
	output string
}{
	{&testEvent{"1", "Add", "This is a test"}, "id: 1\nevent: Add\ndata: This is a test\n\n"},
	{&testEvent{"", "", "This message, it\nhas two lines."}, "data: This message, it\ndata: has two lines.\n\n"},
	{&testEvent{"2", "Add", "This is another test"}, "id: 2\n: This is a comment\nevent: Add\ndata: This is another test\n\n"},
}

func TestRoundTrip(t *testing.T) {
	buf := new(bytes.Buffer)
	enc := NewEncoder(buf, false)
	dec := NewDecoder(buf)
	for _, tt := range encoderTests {
		want := tt.event
		if err := enc.Encode(want); err != nil {
			t.Fatal(err)
		}
		ev, _, err := dec.Decode(0 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Id() != want.Id() || ev.Event() != want.Event() || ev.Data() != want.Data() {
			t.Errorf("Expected: %s %s %s Got: %s %s %s", want.Id(), want.Event(), want.Data(), ev.Id(), ev.Event(), ev.Data())
		}
	}
}
