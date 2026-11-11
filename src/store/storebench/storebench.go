// Package storebench runs a store.Store through a session of generated
// turns and measures what a backend is chosen by: appends and fsyncs per
// turn, bytes written and bytes on disk, the time to load a long session,
// and a keyword scan over its full history.
package storebench

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Subject is a store to measure. The functions supply what the Store
// interface does not expose and each backend answers in its own way.
type Subject struct {
	Name  string
	Store store.Store
	// Reopen returns a store over the same data that shares no memory with
	// Store, as a new process would have. Loads and scans run on it.
	Reopen func() (store.Store, error)
	// DiskBytes returns the bytes the session occupies.
	DiskBytes func(id uuid.UUID) (int64, error)
	// Counters returns the fsyncs issued and the bytes written since the
	// store was made. Nil leaves those columns empty.
	Counters func() (syncs, written int64)
	// Repeats is how many times each read is timed. The median is reported.
	Repeats int
}

// Report is what one run measured.
type Report struct {
	Name    string
	Turns   int
	Entries int
	// PayloadBytes is the size of the entries as compact JSON, one per line.
	PayloadBytes int64
	Appends      int
	// Syncs and WrittenBytes are zero when the subject has no counters.
	Syncs        int64
	WrittenBytes int64
	DiskBytes    int64
	AppendMean   time.Duration
	AppendP50    time.Duration
	AppendP99    time.Duration
	// HeapInuseBytes is the heap in use after the appends and a collection,
	// with the store still open and the generated turns released.
	HeapInuseBytes uint64

	// Load is the time to load the session and read every entry in context.
	Load           time.Duration
	LoadEntries    int
	LoadAllocBytes uint64
	// History is the time to read every entry ever appended.
	History time.Duration
	// Tail is the time to read the last hundredth of the history by range.
	Tail time.Duration
	// Scan is the time to read the full history and search its text.
	Scan           time.Duration
	ScanHits       int
	ScanAllocBytes uint64
}

// Run writes the workload's session to the subject's store and measures it.
func Run(ctx context.Context, sub Subject, w Workload) (Report, error) {
	if sub.Store == nil || sub.Reopen == nil || sub.DiskBytes == nil {
		return Report{}, errors.New("storebench: the subject needs Store, Reopen and DiskBytes")
	}
	if sub.Repeats <= 0 {
		return Report{}, fmt.Errorf("storebench: repeats: %d is not positive", sub.Repeats)
	}
	gen, err := NewGenerator(w)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Name: sub.Name, Turns: w.Turns}

	// Generate first, so that the appends are timed without it.
	turns := make([][]transcript.Entry, w.Turns)
	var counter countWriter
	for i := range turns {
		if turns[i], err = gen.Turn(); err != nil {
			return Report{}, err
		}
		for _, e := range turns[i] {
			if err := json.MarshalWrite(&counter, e); err != nil {
				return Report{}, err
			}
			counter.n++ // the newline
		}
		rep.Entries += len(turns[i])
	}
	rep.PayloadBytes = counter.n

	id := uuid.NewV7()
	created := turns[0][0].Time
	if err := sub.Store.Create(ctx, store.Meta{ID: id, Cwd: "/bench", Created: created, Updated: created}); err != nil {
		return Report{}, err
	}
	var syncs0, written0 int64
	if sub.Counters != nil {
		syncs0, written0 = sub.Counters()
	}
	var total time.Duration
	times := make([]time.Duration, 0, w.Turns)
	for i, turn := range turns {
		start := time.Now()
		if w.AppendPerEntry {
			for j := range turn {
				if err := sub.Store.Append(ctx, id, turn[j:j+1]); err != nil {
					return Report{}, err
				}
			}
			rep.Appends += len(turn)
		} else {
			if err := sub.Store.Append(ctx, id, turn); err != nil {
				return Report{}, err
			}
			rep.Appends++
		}
		elapsed := time.Since(start)
		times = append(times, elapsed)
		total += elapsed
		turns[i] = nil
	}
	if sub.Counters != nil {
		syncs, written := sub.Counters()
		rep.Syncs, rep.WrittenBytes = syncs-syncs0, written-written0
	}
	slices.Sort(times)
	rep.AppendMean = total / time.Duration(len(times))
	rep.AppendP50 = times[len(times)/2]
	rep.AppendP99 = times[len(times)*99/100]
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	rep.HeapInuseBytes = mem.HeapInuse
	if rep.DiskBytes, err = sub.DiskBytes(id); err != nil {
		return Report{}, err
	}

	rep.Load, rep.LoadAllocBytes, rep.LoadEntries, err = timed(sub, func(s store.Store) (int, error) {
		_, entries, err := s.Load(ctx, id)
		if err != nil {
			return 0, err
		}
		return drain(entries, "")
	})
	if err != nil {
		return Report{}, err
	}
	var n int
	rep.History, _, n, err = timed(sub, func(s store.Store) (int, error) {
		entries, err := s.History(ctx, id, store.Range{})
		if err != nil {
			return 0, err
		}
		return drain(entries, "")
	})
	if err != nil {
		return Report{}, err
	}
	if n != rep.Entries {
		return Report{}, fmt.Errorf("storebench: the history has %d entries and %d were appended", n, rep.Entries)
	}
	tail := store.Range{From: rep.Entries - rep.Entries/100}
	rep.Tail, _, n, err = timed(sub, func(s store.Store) (int, error) {
		entries, err := s.History(ctx, id, tail)
		if err != nil {
			return 0, err
		}
		return drain(entries, "")
	})
	if err != nil {
		return Report{}, err
	}
	if n != rep.Entries-tail.From {
		return Report{}, fmt.Errorf("storebench: the range from %d has %d entries of %d", tail.From, n, rep.Entries)
	}
	if w.Keyword != "" {
		rep.Scan, rep.ScanAllocBytes, rep.ScanHits, err = timed(sub, func(s store.Store) (int, error) {
			entries, err := s.History(ctx, id, store.Range{})
			if err != nil {
				return 0, err
			}
			return drain(entries, w.Keyword)
		})
		if err != nil {
			return Report{}, err
		}
	}
	return rep, nil
}

// timed runs read on a reopened store Repeats times and returns the median
// time, the bytes the median run allocated and the count read returned.
func timed(sub Subject, read func(store.Store) (int, error)) (time.Duration, uint64, int, error) {
	type run struct {
		elapsed time.Duration
		alloc   uint64
	}
	runs := make([]run, 0, sub.Repeats)
	count := 0
	for range sub.Repeats {
		s, err := sub.Reopen()
		if err != nil {
			return 0, 0, 0, err
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		if count, err = read(s); err != nil {
			return 0, 0, 0, err
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		runs = append(runs, run{elapsed, after.TotalAlloc - before.TotalAlloc})
	}
	slices.SortFunc(runs, func(a, b run) int { return int(a.elapsed - b.elapsed) })
	median := runs[len(runs)/2]
	return median.elapsed, median.alloc, count, nil
}

// drain reads every entry. With no keyword it counts them, and with one it
// counts the entries whose text holds it.
func drain(entries iter.Seq2[transcript.Entry, error], keyword string) (int, error) {
	n := 0
	for e, err := range entries {
		if err != nil {
			return 0, err
		}
		if keyword == "" || holds(e.Item, keyword) {
			n++
		}
	}
	return n, nil
}

// holds reports whether the text of an item contains keyword.
func holds(item transcript.Item, keyword string) bool {
	parts := func(parts []transcript.Part) bool {
		for _, p := range parts {
			if strings.Contains(p.Text, keyword) || (p.Resource != nil && strings.Contains(p.Resource.Text, keyword)) {
				return true
			}
		}
		return false
	}
	switch {
	case item.User != nil:
		return parts(item.User.Parts)
	case item.Text != nil:
		return strings.Contains(item.Text.Text, keyword)
	case item.Thinking != nil:
		return strings.Contains(item.Thinking.Text, keyword)
	case item.ToolCall != nil:
		return strings.Contains(string(item.ToolCall.Input), keyword)
	case item.ToolResult != nil:
		return parts(item.ToolResult.Parts)
	case item.Compaction != nil:
		return strings.Contains(item.Compaction.Summary, keyword)
	}
	return false
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// WriteTable writes the reports as a Markdown table, one row each.
func WriteTable(w io.Writer, reports []Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)
	var rows strings.Builder
	fmt.Fprintln(&rows, "| subject\t| entries\t| payload MiB\t| appends/turn\t| fsyncs/turn\t| written/payload\t| disk MiB\t| disk/payload\t| append mean\t| p50\t| p99\t| heap MiB\t| load\t| loaded\t| load alloc MiB\t| history\t| tail\t| scan\t| hits\t|")
	fmt.Fprintln(&rows, "|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|---\t|")
	const mib = 1 << 20
	for _, r := range reports {
		turns, payload := float64(r.Turns), float64(r.PayloadBytes)
		syncs, written := "-", "-"
		if r.WrittenBytes > 0 {
			syncs = fmt.Sprintf("%.2f", float64(r.Syncs)/turns)
			written = fmt.Sprintf("%.3f", float64(r.WrittenBytes)/payload)
		}
		fmt.Fprintf(&rows, "| %s\t| %d\t| %.1f\t| %.2f\t| %s\t| %s\t| %.1f\t| %.3f\t| %s\t| %s\t| %s\t| %.1f\t| %s\t| %d\t| %.1f\t| %s\t| %s\t| %s\t| %d\t|\n",
			r.Name, r.Entries, payload/mib, float64(r.Appends)/turns, syncs, written,
			float64(r.DiskBytes)/mib, float64(r.DiskBytes)/payload,
			round(r.AppendMean), round(r.AppendP50), round(r.AppendP99), float64(r.HeapInuseBytes)/mib,
			round(r.Load), r.LoadEntries, float64(r.LoadAllocBytes)/mib, round(r.History), round(r.Tail), round(r.Scan), r.ScanHits)
	}
	if _, err := io.WriteString(tw, rows.String()); err != nil {
		return err
	}
	return tw.Flush()
}

// round keeps three digits of a duration, which is all a run repeats.
func round(d time.Duration) time.Duration {
	unit := time.Nanosecond
	for d/unit >= 1000 {
		unit *= 10
	}
	return d.Round(unit)
}
