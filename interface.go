// Package eventsource implements a client and server to allow streaming data one-way over a HTTP connection
// using the Server-Sent Events API http://dev.w3.org/html5/eventsource/
//
// The client and server respect the Last-Event-ID header.
// If the Repository interface is implemented on the server, events can be replayed in case of a network disconnection.
package eventsource

// Event is the interface for any event received by the client or sent by the server.
type Event interface {
	// Id is an identifier that can be used to allow a client to replay
	// missed Events by returning the Last-Event-Id header.
	// Return empty string if not required.
	Id() string
	// The name of the event. Return empty string if not required.
	Event() string
	// The payload of the event.
	Data() string
}

// Repository is an interface to be used with Server.Register() allowing clients to replay previous events
// through the server, if history is required.
type Repository interface {
	// Gets the Events which should follow on from the specified channel and event id. This method may be called
	// from different goroutines, so it must be safe for concurrent access.
	Replay(channel, id string) chan Event
}

// Logger is the interface for a custom logging implementation that can handle log output for a Stream.
type Logger interface {
	Println(...interface{})
	Printf(string, ...interface{})
}

// StreamErrorHandlerResult is a constant type for use with StreamErrorHandler.
type StreamErrorHandlerResult string

const (
	// StreamErrorProceed is returned by a StreamOptionErrorHandler if the Stream should handle the error
	// normally.
	//
	// If the error represents the failure of an existing connection, Stream will retry the connection. If
	// the error happened during initialization of the Stream, then whether Stream will retry or not is
	// configurable; see StreamOptionCanRetryFirstConnection.
	//
	// Stream will not push the error onto the Errors channel; if you have specified a StreamErrorHandler,
	// the Errors channel is never used.
	//
	// This is the default behavior, so if the handler returns an unknown value it will be treated the
	// same as StreamErrorProceed.
	StreamErrorProceed StreamErrorHandlerResult = "proceed"
	// StreamErrorStop is returned by a StreamConnectionErrorFilter if the Stream should handle the error
	// by immediately stopping and not retrying, as if Close had been called.
	//
	// If the error occurred during initialization of the Stream, rather than on an existing connection,
	// this will also result in the Subscribe function immediately returning the error.
	StreamErrorStop StreamErrorHandlerResult = "stop"
)

// StreamErrorHandler is a function type used with StreamOptionErrorHandler.
//
// This function will be called whenever Stream encounters either a network error or an HTTP error response
// status. The returned value determines whether Stream should retry as usual, or immediately stop.
//
// The error may be any I/O error returned by Go's networking types, or it may be the eventsource type
// SubscriptionError representing an HTTP error response status.
//
// For errors during initialization of the Stream, this function will be called on the same goroutine that
// called the Subscribe method; for errors on an existing connection, it will be called on a worker
// goroutine. It should return promptly and not block the goroutine.
type StreamErrorHandler func(error) StreamErrorHandlerResult
