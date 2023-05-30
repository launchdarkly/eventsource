package eventsource

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		rawInput     string
		wantedEvents []*publication
	}{
		{
			rawInput:     "event: eventName\ndata: {\"sample\":\"value\"}\n\n",
			wantedEvents: []*publication{newPublicationEvent("", "eventName", "", "{\"sample\":\"value\"}")},
		},
		{
			// the newlines should not be parsed as empty event
			rawInput:     "\n\n\nevent: event1\n\n\n\n\nevent: event2\n\n",
			wantedEvents: []*publication{newPublicationEvent("", "event1", "", ""), newPublicationEvent("", "event2", "", "")},
		},
		{
			rawInput:     "id: abc\ndata: def\n\n",
			wantedEvents: []*publication{newPublicationEvent("abc", "", "abc", "def")},
		},
		{
			// id field should be ignored if it contains a null
			rawInput:     "id: a\x00bc\ndata: def\n\n",
			wantedEvents: []*publication{newPublicationEvent("", "", "", "def")},
		},
	}

	for _, test := range tests {
		decoder := NewDecoder(strings.NewReader(test.rawInput))
		i := 0
		for {
			event, err := decoder.Decode()
			if err == io.EOF {
				break
			}
			require.NoError(t, err, "for input: %q", test.rawInput)
			assert.Equal(t, test.wantedEvents[i], event, "for input: %q", test.rawInput)
			i++
		}
		assert.Equal(t, len(test.wantedEvents), i, "Wrong number of decoded events")
	}
}

func requireLastEventID(t *testing.T, event Event) string {
	// necessary because we can't yet add LastEventID to the basic Event interface; see EventWithLastID
	eventWithID, ok := event.(EventWithLastID)
	require.True(t, ok, "event should have implemented EventWithLastID")
	return eventWithID.LastEventID()
}

func TestDecoderTracksLastEventID(t *testing.T) {
	t.Run("uses last ID that is passed in options", func(t *testing.T) {
		inputData := "data: abc\n\n"
		decoder := NewDecoderWithOptions(strings.NewReader(inputData), DecoderOptionLastEventID("my-id"))

		event, err := decoder.Decode()
		require.NoError(t, err)

		evData, err := io.ReadAll(event.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "abc", string(evData))
		assert.Equal(t, "", event.Id())
		assert.Equal(t, "my-id", requireLastEventID(t, event))
	})

	t.Run("last ID persists if not overridden", func(t *testing.T) {
		inputData := "id: abc\ndata: first\n\ndata: second\n\nid: def\ndata:third\n\n"
		decoder := NewDecoderWithOptions(strings.NewReader(inputData), DecoderOptionLastEventID("my-id"))

		event1, err := decoder.Decode()
		require.NoError(t, err)

		evData1, err := io.ReadAll(event1.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "first", string(evData1))
		assert.Equal(t, "abc", event1.Id())
		assert.Equal(t, "abc", requireLastEventID(t, event1))

		event2, err := decoder.Decode()
		require.NoError(t, err)

		evData2, err := io.ReadAll(event2.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "second", string(evData2))
		assert.Equal(t, "", event2.Id())
		assert.Equal(t, "abc", requireLastEventID(t, event2))

		event3, err := decoder.Decode()
		require.NoError(t, err)

		evData3, err := io.ReadAll(event3.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "third", string(evData3))
		assert.Equal(t, "def", event3.Id())
		assert.Equal(t, "def", requireLastEventID(t, event3))
	})

	t.Run("last ID persists if not overridden", func(t *testing.T) {
		inputData := "id: abc\ndata: first\n\ndata: second\n\nid: def\ndata:third\n\n"
		decoder := NewDecoderWithOptions(strings.NewReader(inputData), DecoderOptionLastEventID("my-id"))

		event1, err := decoder.Decode()
		require.NoError(t, err)

		evData1, err := io.ReadAll(event1.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "first", string(evData1))
		assert.Equal(t, "abc", event1.Id())
		assert.Equal(t, "abc", requireLastEventID(t, event1))

		event2, err := decoder.Decode()
		require.NoError(t, err)

		evData2, err := io.ReadAll(event2.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "second", string(evData2))
		assert.Equal(t, "", event2.Id())
		assert.Equal(t, "abc", requireLastEventID(t, event2))

		event3, err := decoder.Decode()
		require.NoError(t, err)

		evData3, err := io.ReadAll(event3.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "third", string(evData3))
		assert.Equal(t, "def", event3.Id())
		assert.Equal(t, "def", requireLastEventID(t, event3))
	})

	t.Run("last ID can be overridden with empty string", func(t *testing.T) {
		inputData := "id: abc\ndata: first\n\nid: \ndata: second\n\n"
		decoder := NewDecoderWithOptions(strings.NewReader(inputData), DecoderOptionLastEventID("my-id"))

		event1, err := decoder.Decode()
		require.NoError(t, err)

		evData1, err := io.ReadAll(event1.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "first", string(evData1))
		assert.Equal(t, "abc", event1.Id())
		assert.Equal(t, "abc", requireLastEventID(t, event1))

		event2, err := decoder.Decode()
		require.NoError(t, err)

		evData2, err := io.ReadAll(event2.GetReader())
		require.NoError(t, err)

		assert.Equal(t, "second", string(evData2))
		assert.Equal(t, "", event2.Id())
		assert.Equal(t, "", requireLastEventID(t, event2))
	})
}
