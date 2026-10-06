package app

import (
	"context"
	"os"
	"slices"
	"time"
)

// fileState captures the attributes used to detect that a file was rebuilt.
type fileState struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime time.Time
}

// fingerprint is the combined state of all watched files.
type fingerprint []fileState

func takeFingerprint(paths []string) fingerprint {
	fp := make(fingerprint, 0, len(paths))
	for _, p := range paths {
		st := fileState{Path: p}
		if info, err := os.Stat(p); err == nil {
			st.Exists = true
			st.Size = info.Size()
			st.ModTime = info.ModTime()
		}
		fp = append(fp, st)
	}
	return fp
}

func (fp fingerprint) Equal(other fingerprint) bool {
	return slices.EqualFunc(fp, other, func(a, b fileState) bool {
		return a.Path == b.Path && a.Exists == b.Exists && a.Size == b.Size &&
			a.ModTime.Equal(b.ModTime)
	})
}

// waitSettled polls the watched files until they stop changing for one
// settle interval, so that a binary still being written by the compiler is
// not executed. It gives up after maxWait and returns the latest state.
func waitSettled(ctx context.Context, paths []string, settle, maxWait time.Duration) fingerprint {
	cur := takeFingerprint(paths)
	if settle <= 0 {
		return cur
	}
	deadline := time.Now().Add(maxWait)
	for {
		select {
		case <-ctx.Done():
			return cur
		case <-time.After(settle):
		}
		next := takeFingerprint(paths)
		if next.Equal(cur) {
			return next
		}
		cur = next
		if time.Now().After(deadline) {
			return cur
		}
	}
}
