package loop

import (
	"slices"
	"sync"

	"github.com/gfx-labs/acpharness/src/tools"
)

// Job is what the scheduler needs to know about one tool call.
type Job struct {
	Effect tools.Effect
	// WritePaths are the absolute paths the call will write.
	WritePaths []string
	// ReadPaths are the absolute paths the call is known to read.
	ReadPaths []string
}

// Conflicts reports whether two calls must not run at the same time:
//
//   - a call that executes runs alone, because what a command touches is
//     not known in advance;
//   - a call that writes without saying where is treated the same way;
//   - two calls that write the same path run one after the other;
//   - a call that reads a path does not overlap one that writes it, so that
//     it sees the file either before or after the write and never half of it.
//
// Calls that only read never conflict.
func Conflicts(a, b Job) bool {
	if exclusive(a) || exclusive(b) {
		return true
	}
	return overlap(a.WritePaths, b.WritePaths) || overlap(a.WritePaths, b.ReadPaths) || overlap(a.ReadPaths, b.WritePaths)
}

func exclusive(j Job) bool {
	return j.Effect == tools.Executes || (j.Effect == tools.Writes && len(j.WritePaths) == 0)
}

func overlap(a, b []string) bool {
	for _, p := range a {
		if slices.Contains(b, p) {
			return true
		}
	}
	return false
}

// Waits returns, for each job, the earlier jobs that must finish before it
// starts. Jobs are in the order the model wrote the calls, and a job only
// ever waits for an earlier one, so conflicting calls run in that order.
func Waits(jobs []Job) [][]int {
	waits := make([][]int, len(jobs))
	for j := range jobs {
		for i := range j {
			if Conflicts(jobs[i], jobs[j]) {
				waits[j] = append(waits[j], i)
			}
		}
	}
	return waits
}

// runAll calls run once for every job, each on its own goroutine, starting
// a job when the jobs it waits for have returned. It returns when every run
// has returned. run decides for itself what a cancelled context means: a
// job is never skipped here, so each one can record its outcome.
func runAll(waits [][]int, run func(i int)) {
	done := make([]chan struct{}, len(waits))
	for i := range done {
		done[i] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i := range waits {
		wg.Go(func() {
			defer close(done[i])
			for _, earlier := range waits[i] {
				<-done[earlier]
			}
			run(i)
		})
	}
	wg.Wait()
}
