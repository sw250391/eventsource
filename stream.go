package eventsource

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"
)

// Stream handles a connection for receiving Server Sent Events.
// It will try and reconnect if the connection is lost, respecting both
// received retry delays and event id's.
type Stream struct {
	c           *http.Client
	req         *http.Request
	lastEventId string
	retry       time.Duration
	// Events emits the events received by the stream
	Events chan Event
	// Errors emits any errors encountered while reading events from the stream.
	// It's mainly for informative purposes - the client isn't required to take any
	// action when an error is encountered. The stream will always attempt to continue,
	// even if that involves reconnecting to the server.
	Errors chan error
	// Logger is a logger that, when set, will be used for logging debug messages
	Logger *log.Logger
	// isClosed is a marker that the stream is/should be closed
	isClosed bool
	// isClosedMutex is a mutex protecting concurrent read/write access of isClosed
	isClosedMutex sync.RWMutex
}

type SubscriptionError struct {
	Code    int
	Message string
}

func (e SubscriptionError) Error() string {
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

// Subscribe to the Events emitted from the specified url.
// If lastEventId is non-empty it will be sent to the server in case it can replay missed events.
func Subscribe(url, lastEventId string) (*Stream, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return SubscribeWithRequest(lastEventId, req)
}

// SubscribeWithRequest will take an http.Request to setup the stream, allowing custom headers
// to be specified, authentication to be configured, etc.
func SubscribeWithRequest(lastEventId string, request *http.Request) (*Stream, error) {
	return SubscribeWith(lastEventId, http.DefaultClient, request)
}

// SubscribeWith takes a http client and request providing customization over both headers and
// control over the http client settings (timeouts, tls, etc)
func SubscribeWith(lastEventId string, client *http.Client, request *http.Request) (*Stream, error) {
	stream := &Stream{
		c:           client,
		req:         request,
		lastEventId: lastEventId,
		retry:       time.Millisecond * 3000,
		Events:      make(chan Event),
		Errors:      make(chan error),
	}
	stream.c.CheckRedirect = checkRedirect

	r, err := stream.connect()
	if err != nil {
		return nil, err
	}
	go stream.stream(r)
	return stream, nil
}

// Close will close the stream. It is safe for concurrent access and can be called multiple times.
func (stream *Stream) Close() {
	if stream.isStreamClosed() {
		return
	}

	stream.markStreamClosed()
	close(stream.Errors)
	close(stream.Events)
}

func (stream *Stream) isStreamClosed() bool {
	stream.isClosedMutex.RLock()
	defer stream.isClosedMutex.RUnlock()
	return stream.isClosed
}

func (stream *Stream) markStreamClosed() {
	stream.isClosedMutex.Lock()
	defer stream.isClosedMutex.Unlock()
	stream.isClosed = true
}

// Go's http package doesn't copy headers across when it encounters
// redirects so we need to do that manually.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	for k, vv := range via[0].Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	return nil
}

func (stream *Stream) connect() (r io.ReadCloser, err error) {
	var resp *http.Response
	stream.req.Header.Set("Cache-Control", "no-cache")
	stream.req.Header.Set("Accept", "text/event-stream")
	if len(stream.lastEventId) > 0 {
		stream.req.Header.Set("Last-Event-ID", stream.lastEventId)
	}
	if resp, err = stream.c.Do(stream.req); err != nil {
		return
	}
	if resp.StatusCode != 200 {
		message, _ := ioutil.ReadAll(resp.Body)
		err = SubscriptionError{
			Code:    resp.StatusCode,
			Message: string(message),
		}
	}
	r = resp.Body
	return
}

func (stream *Stream) stream(r io.ReadCloser) {
	defer r.Close()

	// receives events until an error is encountered
	stream.receiveEvents(r)

	// tries to reconnect and start the stream again
	stream.retryRestartStream()
}

func (stream *Stream) receiveEvents(r io.ReadCloser) {
	dec := NewDecoder(r)

	for {
		ev, err := dec.Decode()
		if stream.isStreamClosed() {
			return
		}
		if err != nil {
			fmt.Println("Stream read returned error %s", err)
			return
		}

		fmt.Println("Id is ", ev.Id())
		fmt.Println("Event is ", ev.Event())
		fmt.Println("Data is ", ev.Data())

		pub := ev.(*publication)
		if pub.Retry() > 0 {
			stream.retry = time.Duration(pub.Retry()) * time.Millisecond
		}
		if len(pub.Id()) > 0 {
			stream.lastEventId = pub.Id()
		}
		stream.Events <- ev
	}
}

func (stream *Stream) retryRestartStream() {
	backoff := stream.retry
	for {
		if stream.Logger != nil {
			stream.Logger.Printf("Reconnecting in %0.4f secs\n", backoff.Seconds())
		}
		time.Sleep(backoff)
		if stream.isStreamClosed() {
			return
		}
		// NOTE: because of the defer we're opening the new connection
		// before closing the old one. Shouldn't be a problem in practice,
		// but something to be aware of.
		r, err := stream.connect()
		if err == nil {
			go stream.stream(r)
			return
		}
		stream.Errors <- err
	}
}
