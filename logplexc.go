// A client implementation that includes concurrency and dropping.
package logplexc

import (
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	// Number of concurrent requests at the time of retrieval.
	Concurrency int32

	// Message-level statistics

	// Total messages submitted
	Total uint64

	// Incremented when a message is ignored outright because of
	// too much work being done already.
	Dropped uint64

	// Incremented when a log post request is not known to have
	// succeeded and one has given up waiting.
	Cancelled uint64

	// Incremented when a log post request is responded to,
	// affirming that the messages have been rejected.
	Rejected uint64

	// Incremented only when a positive response is received from
	// logplex.
	Successful uint64

	// Request-level statistics

	TotalRequests   uint64
	DroppedRequests uint64
	CancelRequests  uint64
	RejectRequests  uint64
	SuccessRequests uint64
}

type TimeTriggerBehavior byte

const (
	// Carefully choose the zero-value so it is a reasonable
	// default, so that a user requesting the other behaviors --
	// which do not need a time -- can write things like:
	// TimeTrigger{Behavior: TimeTriggerImmediate} without
	// specifying a Period.
	TimeTriggerPeriodic TimeTriggerBehavior = iota
	TimeTriggerImmediate
	TimeTriggerNever
)

type Client struct {
	Stats
	statLock sync.Mutex

	c *MiniClient

	// Concurrency control of POST workers: the current level of
	// concurrency, and a token bucket channel.
	concurrency int32
	bucket      chan struct{}

	// Threshold of logplex request size to trigger POST.
	RequestSizeTrigger int

	// For implementing timely flushing of log buffers.
	timeTrigger TimeTriggerBehavior
	ticker      *time.Ticker

	// Closed when cleaning up
	finalize     chan struct{}
	finalizeDone sync.WaitGroup
}

type Config struct {
	Logplex            url.URL
	Token              string
	HttpClient         http.Client
	RequestSizeTrigger int
	Concurrency        int
	Period             time.Duration

	// Optional: Can be set for advanced behaviors like triggering
	// Never or Immediately.
	TimeTrigger TimeTriggerBehavior
}

func NewClient(cfg *Config) (*Client, error) {
	c, err := NewMiniClient(
		&MiniConfig{
			Logplex:    cfg.Logplex,
			Token:      cfg.Token,
			HttpClient: cfg.HttpClient,
		})

	if err != nil {
		return nil, err
	}

	m := Client{
		c:                  c,
		finalize:           make(chan struct{}),
		bucket:             make(chan struct{}),
		RequestSizeTrigger: cfg.RequestSizeTrigger,
	}

	// Handle determining m.timeTrigger.  This complexity seems
	// reasonable to allow the user to get some input checking
	// (negative Periods) and to get TimeTriggerImmediate by
	// passing a zero-duration period (TimeTriggerImmediate is
	// still useful for internal bookkeeping).
	switch cfg.TimeTrigger {
	case TimeTriggerPeriodic:
		if cfg.Period < 0 {
			return nil, errors.New(
				"logplexc.Client: negative target " +
					"latency not allowed")
		} else if cfg.Period == 0 {
			// Rewrite a zero-duration period into an
			// immediate flush.
			m.timeTrigger = TimeTriggerImmediate
		} else if cfg.Period > 0 {
			m.timeTrigger = TimeTriggerPeriodic
		} else {
			panic("bug")
		}
	default:
		m.timeTrigger = cfg.TimeTrigger
	}

	// Supply tokens to the buckets.
	//
	// This goroutine exits when it has supplied all of the
	// initial tokens: that's because worker goroutines are
	// responsible for re-inserting tokens.
	m.finalizeDone.Add(1)
	go func() {
		defer func() { m.finalizeDone.Done() }()

		for i := 0; i < cfg.Concurrency; i += 1 {
			select {
			case m.bucket <- struct{}{}:
			case <-m.finalize:
				return
			}
		}
	}()

	// Set up the time-based log flushing, if requested.
	if m.timeTrigger == TimeTriggerPeriodic {
		m.ticker = time.NewTicker(cfg.Period)

		m.finalizeDone.Add(1)
		go func() {
			defer func() { m.finalizeDone.Done() }()

			for {
				// Wait for a while to do work, or to
				// exit
				select {
				case <-m.ticker.C:
				case <-m.finalize:
					return
				}

				m.maybeWork()
			}
		}()
	}

	return &m, nil
}

func (m *Client) Close() {
	// Clean up otherwise immortal ticker goroutine
	m.ticker.Stop()
	close(m.finalize)
	m.finalizeDone.Wait()
}

func (m *Client) BufferMessage(
	when time.Time, host string, procId string, log []byte) error {

	select {
	case <-m.finalize:
		return errors.New("Failed trying to buffer a message: " +
			"client already Closed")
	default:
		// no-op
	}

	s := m.c.BufferMessage(when, host, procId, log)
	if s.Buffered >= m.RequestSizeTrigger ||
		m.timeTrigger == TimeTriggerImmediate {
		m.maybeWork()
	}

	return nil
}

func (m *Client) Statistics() (s Stats) {
	m.statLock.Lock()
	defer m.statLock.Unlock()

	s = m.Stats
	return s
}

func (m *Client) maybeWork() {
	atomic.AddInt32(&m.Stats.Concurrency, 1)
	defer atomic.AddInt32(&m.Stats.Concurrency, -1)

	b := m.c.SwapBundle()

	// Avoid sending empty requests
	if b.NumberFramed <= 0 {
		return
	}

	// Check if there are any worker tokens available. If not,
	// then just abort after recording drop statistics.
	select {
	case <-m.bucket:
		m.finalizeDone.Add(1)
		go m.syncWorker(&b)

	default:
		m.statReqDrop(&b.MiniStats)

		// In GOMAXPROCS=1 cases, tight loops can starve out
		// any of the workers predictably and seemingly
		// forever.
		runtime.Gosched()
	}
}

func (m *Client) syncWorker(b *Bundle) {
	defer func() { m.finalizeDone.Done() }()

	// When exiting, free up the token for use by another
	// worker.
	defer func() {
		select {
		case m.bucket <- struct{}{}:
			// Made token available.
		case <-m.finalize:
			// Client is shutting down, allow termination
			// from the closed finalize.
		}
	}()

	// Post to logplex.
	resp, err := m.c.Post(b)
	if err != nil {
		m.statReqErr(&b.MiniStats)
		return
	}

	defer resp.Body.Close()

	// Check HTTP return code and accrue statistics accordingly.
	if resp.StatusCode != http.StatusNoContent {
		m.statReqRej(&b.MiniStats)
	} else {
		m.statReqSuccess(&b.MiniStats)
	}
}

func (m *Client) statReqTotalUnsync(s *MiniStats) {
	m.Total += s.NumberFramed
	m.TotalRequests += 1
}

func (m *Client) statReqSuccess(s *MiniStats) {
	m.statLock.Lock()
	defer m.statLock.Unlock()
	m.statReqTotalUnsync(s)

	m.Successful += s.NumberFramed
	m.SuccessRequests += 1
}

func (m *Client) statReqErr(s *MiniStats) {
	m.statLock.Lock()
	defer m.statLock.Unlock()
	m.statReqTotalUnsync(s)

	m.Cancelled += s.NumberFramed
	m.CancelRequests += 1
}

func (m *Client) statReqRej(s *MiniStats) {
	m.statLock.Lock()
	defer m.statLock.Unlock()
	m.statReqTotalUnsync(s)

	m.Rejected += s.NumberFramed
	m.RejectRequests += 1
}

func (m *Client) statReqDrop(s *MiniStats) {
	m.statLock.Lock()
	defer m.statLock.Unlock()
	m.statReqTotalUnsync(s)

	m.Dropped += s.NumberFramed
	m.DroppedRequests += 1
}
