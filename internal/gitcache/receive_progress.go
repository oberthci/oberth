package gitcache

import (
	"io"
	"sync"
)

// Once a hook calls the server, Git has consumed the push. Only response
// writes can then wait on the client; hooks and publication use the total bound.
type receiveProgress struct {
	mu         sync.Mutex
	progress   *commandProgress
	begin, end func()
	processing bool
	writers    int
}

func (p *receiveProgress) process() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.processing {
		p.processing = true
		if p.writers == 0 {
			p.pause()
		}
	}
}

func (p *receiveProgress) pause() {
	p.progress.pause()
	if p.end != nil {
		p.end()
	}
}

func (p *receiveProgress) write(start bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if start {
		if p.processing && p.writers == 0 {
			p.progress.resume()
			if p.begin != nil {
				p.begin()
			}
		}
		p.writers++
	} else {
		p.writers--
		if p.processing && p.writers == 0 {
			p.pause()
		}
	}
}

func (p *receiveProgress) writer(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return receiveWriter{Writer: w, progress: p}
}

type receiveWriter struct {
	io.Writer
	progress *receiveProgress
}

func (w receiveWriter) Write(b []byte) (int, error) {
	w.progress.write(true)
	defer w.progress.write(false)
	return w.Writer.Write(b)
}
