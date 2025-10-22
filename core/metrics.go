package core

import (
	"io"
	"net/http"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

type Metrics struct {
	SentBytes     atomic.Uint64
	ReceivedBytes atomic.Uint64
	SentCount     atomic.Uint64
	ReceivedCount atomic.Uint64
}

func NewMetrics() *Metrics {
	return &Metrics{}
}

type InstrumentedRoundTripper struct {
	http.RoundTripper
	metrics *Metrics
}

func NewInstrumentedRoundTripper(rt http.RoundTripper, metrics *Metrics) *InstrumentedRoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &InstrumentedRoundTripper{
		RoundTripper: rt,
		metrics:      metrics,
	}
}

func (i *InstrumentedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	i.metrics.SentCount.Add(1)

	if req.Body != nil {
		req.Body = &countingReadCloser{
			ReadCloser: req.Body,
			counter:    &i.metrics.SentBytes,
		}
	}

	resp, err := i.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	i.metrics.ReceivedCount.Add(1)

	if resp.Body != nil {
		resp.Body = &countingReadCloser{
			ReadCloser: resp.Body,
			counter:    &i.metrics.ReceivedBytes,
		}
	}

	return resp, nil
}

type InstrumentedConn struct {
	*websocket.Conn
	metrics *Metrics
}

func NewInstrumentedConn(conn *websocket.Conn, metrics *Metrics) *InstrumentedConn {
	return &InstrumentedConn{
		Conn:    conn,
		metrics: metrics,
	}
}

func (ic *InstrumentedConn) NextWriter(messageType int) (io.WriteCloser, error) {
	ic.metrics.SentCount.Add(1)
	w, err := ic.Conn.NextWriter(messageType)
	if err != nil {
		return nil, err
	}
	return &countingWriteCloser{WriteCloser: w, counter: &ic.metrics.SentBytes}, nil
}

func (ic *InstrumentedConn) NextReader() (int, io.Reader, error) {
	messageType, r, err := ic.Conn.NextReader()
	if err == nil {
		ic.metrics.ReceivedCount.Add(1)
		return messageType, &countingReader{Reader: r, counter: &ic.metrics.ReceivedBytes}, nil
	}
	return messageType, r, err
}

type countingReadCloser struct {
	io.ReadCloser
	counter *atomic.Uint64
}

func (r *countingReadCloser) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	if n > 0 {
		r.counter.Add(uint64(n))
	}
	return n, err
}

type countingWriteCloser struct {
	io.WriteCloser
	counter *atomic.Uint64
}

func (cw *countingWriteCloser) Write(p []byte) (int, error) {
	n, err := cw.WriteCloser.Write(p)
	if n > 0 {
		cw.counter.Add(uint64(n))
	}
	return n, err
}

type countingReader struct {
	io.Reader
	counter *atomic.Uint64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.Reader.Read(p)
	if n > 0 {
		cr.counter.Add(uint64(n))
	}
	return n, err
}
