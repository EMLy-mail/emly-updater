package source

import (
	"context"
	"io"
)

// ProgressFunc receives a setup download's progress: done bytes so far out
// of total, which is -1 when the server sent no Content-Length. It is called
// once with done = 0 as soon as the server has accepted the download (HTTP
// 200) and then after every read, so it must be cheap - throttling is the
// receiver's job.
type ProgressFunc func(done, total int64)

type progressKey struct{}

// WithProgress returns a ctx whose FetchSetup calls report to fn.
//
// It travels in the context rather than in the Source interface because only
// the caller of download.Manager.Ensure knows whether anyone is watching, and
// the Manager, its pacer and every Source fake in the tests stay unaware of it.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressFrom(ctx context.Context) ProgressFunc {
	fn, _ := ctx.Value(progressKey{}).(ProgressFunc)
	return fn
}

// progressReader reports every read of r to fn.
type progressReader struct {
	r     io.Reader
	fn    ProgressFunc
	done  int64
	total int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		p.fn(p.done, p.total)
	}
	return n, err
}
