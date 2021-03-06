package peco

import (
	"bufio"
	"io"
	"sync"
	"time"

	"context"

	"github.com/lestrrat/go-pdebug"
	"github.com/peco/peco/internal/util"
	"github.com/peco/peco/line"
	"github.com/peco/peco/pipeline"
)

// Creates a new Source. Does not start processing the input until you
// call Setup()
func NewSource(in io.Reader, idgen line.IDGenerator, capacity int, enableSep bool) *Source {
	s := &Source{
		in:            in, // Note that this may be closed, so do not rely on it
		capacity:      capacity,
		enableSep:     enableSep,
		idgen:         idgen,
		ready:         make(chan struct{}),
		setupDone:     make(chan struct{}),
		setupOnce:     sync.Once{},
		ChanOutput: pipeline.ChanOutput(make(chan interface{})),
	}
	s.Reset()
	return s
}

// Setup reads from the input os.File.
func (s *Source) Setup(ctx context.Context, state *Peco) {
	s.setupOnce.Do(func() {
		done := make(chan struct{})
		refresh := make(chan struct{}, 1)
		defer close(done)
		defer close(refresh)
		// And also, close the done channel so we can tell the consumers
		// we have finished reading everything
		defer close(s.setupDone)

		draw := func(state *Peco) {
			state.Hub().SendDraw(nil)
		}

		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-done:
					draw(state)
					return
				case <-ticker.C:
					draw(state)
				}
			}
		}()

		// This sync.Once var is used to receive the notification
		// that there was at least 1 line read from the source
		// This is wrapped in a sync.Notify so we can safely call
		// it in multiple places
		var notify sync.Once
		notifycb := func() {
			// close the ready channel so others can be notified
			// that there's at least 1 line in the buffer
			state.Hub().SendStatusMsg("")
			close(s.ready)
		}

		// Register this to be called in a defer, just in case we could bailed
		// out without reading a single line.
		// Note: this will be a no-op if notify.Do has been called before
		defer notify.Do(notifycb)

		scanner := bufio.NewScanner(s.in)
		defer func() {
			if util.IsTty(s.in) {
				return
			}
			if closer, ok := s.in.(io.Closer); ok {
				closer.Close()
			}
		}()

		lines := make(chan string)
		go func() {
			defer close(lines)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()

		state.Hub().SendStatusMsg("Waiting for input...")

		readCount := 0
		for loop := true; loop; {
			select {
			case <-ctx.Done():
				if pdebug.Enabled {
					pdebug.Printf("Bailing out of source setup, because ctx was canceled")
				}
				return
			case l, ok := <-lines:
				if !ok {
					if pdebug.Enabled {
						pdebug.Printf("No more lines to read...")
					}
					loop = false
					break
				}

				readCount++
				s.Append(line.NewRaw(s.idgen.Next(), l, s.enableSep))
				notify.Do(notifycb)
			}
		}

		if pdebug.Enabled {
			pdebug.Printf("Read all %d lines from source", readCount)
		}
	})
}

// Start starts
func (s *Source) Start(ctx context.Context, out pipeline.ChanOutput) {
	// I should be the only one running this method until I bail out
	if pdebug.Enabled {
		g := pdebug.Marker("Source.Start")
		defer g.End()
		defer pdebug.Printf("Source sent %d lines", len(s.lines))
	}

	for _, l := range s.lines {
		select {
		case <-ctx.Done():
			if pdebug.Enabled {
				pdebug.Printf("Source: context.Done detected")
			}
			return
		default:
			out.Send(l)
		}
	}
	out.SendEndMark("end of input")
}

// Reset resets the state of the source object so that it
// is ready to feed the filters
func (s *Source) Reset() {
	if pdebug.Enabled {
		g := pdebug.Marker("Source.Reset")
		defer g.End()
	}
	s.ChanOutput = pipeline.ChanOutput(make(chan interface{}))
}

// Ready returns the "input ready" channel. It will be closed as soon as
// the first line of input is processed via Setup()
func (s *Source) Ready() <-chan struct{} {
	return s.ready
}

// SetupDone returns the "read all lines" channel. It will be closed as soon as
// the all input has been read
func (s *Source) SetupDone() <-chan struct{} {
	return s.setupDone
}

func (s *Source) LineAt(n int) (line.Line, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return bufferLineAt(s.lines, n)
}

func (s *Source) Size() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return bufferSize(s.lines)
}

func (s *Source) Append(l line.Line) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.lines = append(s.lines, l)
	if s.capacity > 0 && len(s.lines) > s.capacity {
		diff := len(s.lines) - s.capacity

		// Golang's version of array realloc
		s.lines = s.lines[diff:s.capacity:s.capacity]
	}
}
