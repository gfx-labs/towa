// Package search finds text and files with ripgrep. The rg binary is
// required: there is no other implementation behind it.
package search

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
)

// stderrKeep is how much of rg's error output is kept. rg reports a bad
// pattern or glob in a few lines.
const stderrKeep = 4 << 10

// Kind says why a search could not be done.
type Kind string

// Kinds of an Error.
const (
	// KindNoRipgrep: the rg binary was not found. Program holds the name
	// that was looked for.
	KindNoRipgrep Kind = "no_ripgrep"
	// KindEmptyPattern: a grep has no pattern.
	KindEmptyPattern Kind = "empty_pattern"
	// KindInvalid: rg rejected the pattern, a glob or a file type. Message
	// holds what rg said.
	KindInvalid Kind = "invalid"
	// KindRange: a limit, an offset or a context count is negative.
	KindRange Kind = "range"
)

// Error is a search that could not be done. A path that cannot be searched
// is an *fsops.Error.
type Error struct {
	Kind    Kind
	Program string
	Message string
	Err     error
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindNoRipgrep:
		return fmt.Sprintf("search: the ripgrep binary %q was not found. Search needs ripgrep: install it or set tools.search.ripgrep to its path: %v", e.Program, e.Err)
	case KindInvalid:
		return "search: " + e.Message
	default:
		return "search: " + string(e.Kind)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Filter narrows the files a search looks at.
type Filter struct {
	// Path is a file or directory. An empty Path is the working directory,
	// and a relative one is taken from it.
	Path string
	// Globs are rg glob patterns. A pattern starting with "!" excludes.
	Globs []string
	// Types are rg file type names, such as "go".
	Types []string
	// Hidden includes files and directories whose name starts with a dot.
	Hidden bool
	// NoIgnore includes files that .gitignore and similar files exclude.
	NoIgnore bool
	// Offset skips that many results. Limit is the most results returned,
	// and zero means no limit other than the configured output size.
	Offset int
	Limit  int
}

// Output says what a grep returns.
type Output int

// Outputs of a grep.
const (
	// Lines returns matching lines and their context.
	Lines Output = iota
	// Files returns the files that have a match.
	Files
	// Counts returns how many lines match in each file.
	Counts
)

// GrepInput is a search for text.
type GrepInput struct {
	Filter
	Pattern string
	// Literal takes Pattern as text. Otherwise it is an rg regular
	// expression.
	Literal    bool
	IgnoreCase bool
	// Multiline lets a pattern match across lines, with "." matching a
	// newline.
	Multiline bool
	// Before and After are lines of context around each match.
	Before int
	After  int
	Output Output
}

// Line is one line of a grep's output.
type Line struct {
	// Path is absolute. It is the bytes of the file name, which need not be
	// UTF-8.
	Path   string
	Number int
	// Text is the line without its line ending. A multiline match holds
	// several lines. Bytes that are not UTF-8 are replaced with U+FFFD.
	Text string
	// Context is set on a line that is near a match and is not one.
	Context bool
}

// File is a file a search found.
type File struct {
	// Path is absolute. It is the bytes of the file name, which need not be
	// UTF-8.
	Path string
	// ModTime is the zero time when the file was gone before it was looked
	// at.
	ModTime time.Time
	// Count is the number of matching lines, for a grep with Counts.
	Count int
}

// Result is what a search found. A grep with Lines fills Lines, and every
// other search fills Files. Both are in path order: a directory's entries
// by name, and a directory's contents where its name sorts.
type Result struct {
	Lines []Line
	Files []File
	// More says there were results after Limit.
	More bool
	// Cut says the search stopped because the results passed the configured
	// output size. What was found before is returned.
	Cut bool
	// Unreadable says rg could not read some of the files.
	Unreadable bool
}

// Grep searches file contents.
func Grep(ctx context.Context, env tools.Env, cfg config.Tools, in GrepInput) (Result, error) {
	if in.Pattern == "" {
		return Result{}, &Error{Kind: KindEmptyPattern}
	}
	if in.Before < 0 || in.After < 0 {
		return Result{}, &Error{Kind: KindRange}
	}
	args := []string{"--no-config", "--no-messages"}
	if in.Literal {
		args = append(args, "--fixed-strings")
	}
	if in.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	if in.Multiline {
		args = append(args, "--multiline", "--multiline-dotall")
	}
	var parse func(*run, io.Reader) error
	switch in.Output {
	case Lines:
		// rg sorts the lines, on one thread, so that the search can stop at
		// the limit without reading every match. A file list is short, so
		// rg searches in parallel and the list is sorted here.
		args = append(args, "--sort", "path", "--json", "--before-context", strconv.Itoa(in.Before), "--after-context", strconv.Itoa(in.After))
		parse = (*run).parseJSON
	case Files:
		args = append(args, "--files-with-matches", "--null")
		parse = (*run).parsePaths
	case Counts:
		args = append(args, "--count", "--null")
		parse = (*run).parseCounts
	default:
		return Result{}, fmt.Errorf("search: output %d is not defined", in.Output)
	}
	args = append(in.args(args), "--regexp", in.Pattern)
	return in.run(ctx, env, cfg, args, parse)
}

// Find lists files by name.
func Find(ctx context.Context, env tools.Env, cfg config.Tools, in Filter) (Result, error) {
	return in.run(ctx, env, cfg, in.args([]string{"--no-config", "--no-messages", "--files", "--null"}), (*run).parsePaths)
}

func (f Filter) args(args []string) []string {
	for _, glob := range f.Globs {
		args = append(args, "--glob", glob)
	}
	for _, name := range f.Types {
		args = append(args, "--type", name)
	}
	if f.Hidden {
		args = append(args, "--hidden")
	}
	if f.NoIgnore {
		args = append(args, "--no-ignore")
	}
	return args
}

// run is one rg process and what has been taken from its output.
type run struct {
	filter Filter
	budget int64
	used   int64
	seen   int
	// files are the files rg has reported, in the order it found them.
	files []File
	res   Result
}

// errStop ends the reading of rg's output early.
var errStop = errors.New("search: enough results")

func (f Filter) run(ctx context.Context, env tools.Env, cfg config.Tools, args []string, parse func(*run, io.Reader) error) (Result, error) {
	if f.Offset < 0 || f.Limit < 0 {
		return Result{}, &Error{Kind: KindRange}
	}
	program, err := exec.LookPath(cfg.Search.Ripgrep)
	if err != nil {
		return Result{}, &Error{Kind: KindNoRipgrep, Program: cfg.Search.Ripgrep, Err: err}
	}
	path := f.Path
	if path == "" {
		path = env.Cwd
	}
	target, err := fsops.RootsOf(env, cfg.Files).Resolve(path)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{}, &fsops.Error{Kind: fsops.KindNotFound, Path: path, Err: err}
		}
		return Result{}, fmt.Errorf("search: %w", err)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	// rg is given an absolute path, so the paths it prints are absolute.
	cmd := exec.CommandContext(runCtx, program, append(args, "--", target)...)
	cmd.Dir = env.Cwd
	stderr := &headWriter{keep: stderrKeep}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("search: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("search: start %s: %w", program, err)
	}
	r := &run{filter: f, budget: cfg.MaxOutputBytes}
	parseErr := parse(r, stdout)
	stopped := errors.Is(parseErr, errStop)
	if parseErr != nil {
		// rg is still writing. Kill it so that Wait returns.
		stop()
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		// The caller cancelled. A partial result would look complete.
		return Result{}, ctx.Err()
	case stopped:
		return r.res, nil
	case parseErr != nil:
		return Result{}, parseErr
	}
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
	case !errors.As(waitErr, &exit):
		return Result{}, fmt.Errorf("search: wait for %s: %w", program, waitErr)
	case exit.ExitCode() == 1:
		// Nothing matched.
	case stderr.buf.Len() > 0:
		// File errors are silenced with --no-messages, so what is left is
		// rg refusing its arguments.
		return Result{}, &Error{Kind: KindInvalid, Message: strings.TrimSpace(stderr.buf.String()), Err: waitErr}
	case exit.ExitCode() == 2:
		r.res.Unreadable = true
	default:
		return Result{}, fmt.Errorf("search: %s: %w", program, waitErr)
	}
	if err := r.keepFiles(); err != nil {
		return Result{}, err
	}
	return r.res, nil
}

// comparePaths orders paths as rg --sort path does: by each name along the
// path in turn, so a directory's contents come where its name sorts. At
// the first byte that differs, a name that has ended is the shorter name.
func comparePaths(a, b string) int {
	for i := range min(len(a), len(b)) {
		switch {
		case a[i] == b[i]:
		case a[i] == os.PathSeparator:
			return -1
		case b[i] == os.PathSeparator:
			return 1
		case a[i] < b[i]:
			return -1
		default:
			return 1
		}
	}
	return len(a) - len(b)
}

// keepFiles sorts the files rg reported and keeps those the offset, the
// limit and the output size allow.
func (r *run) keepFiles() error {
	slices.SortFunc(r.files, func(a, b File) int { return comparePaths(a.Path, b.Path) })
	for _, file := range r.files {
		keep, err := r.take(len(file.Path))
		if keep {
			switch info, statErr := os.Stat(file.Path); {
			case statErr == nil:
				file.ModTime = info.ModTime()
			case errors.Is(statErr, os.ErrNotExist):
			default:
				return fmt.Errorf("search: %w", statErr)
			}
			r.res.Files = append(r.res.Files, file)
		}
		if err != nil {
			break
		}
	}
	return nil
}

// take says whether the next result is kept, and returns errStop when no
// more are wanted. size is what the result adds to the output.
func (r *run) take(size int) (bool, error) {
	r.seen++
	if r.seen <= r.filter.Offset {
		return false, nil
	}
	if r.filter.Limit > 0 && r.seen > r.filter.Offset+r.filter.Limit {
		r.res.More = true
		return false, errStop
	}
	if r.used+int64(size) > r.budget {
		r.res.Cut = true
		return false, errStop
	}
	r.used += int64(size)
	return true, nil
}

// rgText is text in rg's JSON output: "text" when it is UTF-8 and base64
// in "bytes" when it is not.
type rgText struct {
	Text  *string `json:"text"`
	Bytes string  `json:"bytes"`
}

func (t rgText) raw() (string, error) {
	if t.Text != nil {
		return *t.Text, nil
	}
	b, err := base64.StdEncoding.DecodeString(t.Bytes)
	if err != nil {
		return "", fmt.Errorf("search: rg output: %w", err)
	}
	return string(b), nil
}

type rgMessage struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		Lines      rgText `json:"lines"`
		LineNumber int    `json:"line_number"`
	} `json:"data"`
}

func (r *run) parseJSON(out io.Reader) error {
	dec := jsontext.NewDecoder(out)
	for {
		var msg rgMessage
		if err := json.UnmarshalDecode(dec, &msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("search: rg output: %w", err)
		}
		if msg.Type != "match" && msg.Type != "context" {
			continue
		}
		path, err := msg.Data.Path.raw()
		if err != nil {
			return err
		}
		text, err := msg.Data.Lines.raw()
		if err != nil {
			return err
		}
		text = strings.ToValidUTF8(strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r"), "\uFFFD")
		keep, err := r.take(len(path) + len(text))
		if keep {
			r.res.Lines = append(r.res.Lines, Line{
				Path: path, Number: msg.Data.LineNumber, Text: text, Context: msg.Type == "context",
			})
		}
		if err != nil {
			return err
		}
	}
}

// records calls fn with each piece of out that ends in sep.
func records(out io.Reader, sep byte, fn func(record []byte) error) error {
	reader := bufio.NewReader(out)
	for {
		record, err := reader.ReadBytes(sep)
		if errors.Is(err, io.EOF) {
			if len(record) > 0 {
				return fmt.Errorf("search: rg output ends inside a record: %q", record)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("search: rg output: %w", err)
		}
		if err := fn(record[:len(record)-1]); err != nil {
			return err
		}
	}
}

func (r *run) parsePaths(out io.Reader) error {
	return records(out, 0, func(record []byte) error {
		r.files = append(r.files, File{Path: string(record)})
		return nil
	})
}

// parseCounts reads "path NUL count LF" records.
func (r *run) parseCounts(out io.Reader) error {
	reader := bufio.NewReader(out)
	for {
		path, err := reader.ReadBytes(0)
		if errors.Is(err, io.EOF) && len(path) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("search: rg output: %w", err)
		}
		number, err := reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("search: rg output: %w", err)
		}
		count, err := strconv.Atoi(string(bytes.TrimSuffix(number, []byte("\n"))))
		if err != nil {
			return fmt.Errorf("search: rg output: %w", err)
		}
		r.files = append(r.files, File{Path: string(path[:len(path)-1]), Count: count})
	}
}

// headWriter keeps the first bytes written to it and accepts the rest.
type headWriter struct {
	keep int
	buf  bytes.Buffer
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.keep - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
