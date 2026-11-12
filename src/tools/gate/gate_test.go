package gate_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/gate"
	"github.com/gfx-labs/towa/src/tools/shell"
	"github.com/gfx-labs/towa/src/transcript"
)

func toolsConfig(t *testing.T) config.Tools {
	t.Helper()
	loaded, err := config.Load(config.Sources{})
	require.NoError(t, err)
	return loaded.Config.Tools
}

// seq runs a real command and returns its output as a tool result.
func seq(t *testing.T, cfg config.Tools, env tools.Env, last int) tools.Result {
	t.Helper()
	res, err := shell.Run(context.Background(), env, cfg, shell.Input{Command: "seq 1 " + strconv.Itoa(last)})
	require.NoError(t, err)
	return tools.Result{Outcome: res.Outcome(), Parts: []transcript.Part{{Text: res.Output}}}
}

// blobDir keeps blobs as files named by their hash, as a store would.
func blobDir(t *testing.T) (string, func(context.Context, string, io.Reader) (transcript.Stored, error)) {
	t.Helper()
	dir := t.TempDir()
	return dir, func(_ context.Context, mimeType string, content io.Reader) (transcript.Stored, error) {
		data, err := io.ReadAll(content)
		if err != nil {
			return transcript.Stored{}, err
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(dir, hash), data, 0o600); err != nil {
			return transcript.Stored{}, err
		}
		return transcript.Stored{Hash: "sha256:" + hash, Size: int64(len(data)), MimeType: mimeType}, nil
	}
}

func TestALargeResultIsWithheldUntilAccepted(t *testing.T) {
	cfg := toolsConfig(t)
	cfg.Gate.BudgetTokens = 100
	dir, put := blobDir(t)
	env := tools.Env{Cwd: t.TempDir(), PutBlob: put}
	ctx := context.Background()

	// seq 1 20 is 51 bytes, 13 tokens at four bytes a token: under budget.
	small := seq(t, cfg, env, 20)
	got, sum := gate.Apply(ctx, env, cfg.Gate, small, false)
	require.Nil(t, sum)
	require.Equal(t, small, got)

	// seq 1 1000 is 3893 bytes: 974 tokens.
	large := seq(t, cfg, env, 1000)
	full := large.Parts[0].Text
	got, sum = gate.Apply(ctx, env, cfg.Gate, large, false)
	require.NotNil(t, sum)
	require.Equal(t, int64(974), sum.EstimatedTokens)
	require.Equal(t, int64(3893), sum.Bytes)
	require.Equal(t, 1000, sum.Lines)
	require.Equal(t, transcript.OutcomeCompleted, got.Outcome)
	// The model gets the summary and a reference, and none of the output.
	require.Len(t, got.Parts, 2)
	require.Equal(t, sum.Text(), got.Parts[0].Text)
	require.NotContains(t, got.Parts[0].Text, "999")
	require.Contains(t, got.Parts[0].Text, "974")
	require.Contains(t, got.Parts[0].Text, gate.AcceptMember)
	stored := got.Parts[1].Stored
	require.NotNil(t, stored)
	kept, err := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(stored.Hash, "sha256:")))
	require.NoError(t, err)
	require.Equal(t, full, string(kept))
	// The client is still shown all of it.
	require.Len(t, got.Content, 1)
	require.Equal(t, full, got.Content[0].Content.Content.Text.Text)

	// Accepted, or gating off: the result is untouched.
	got, sum = gate.Apply(ctx, env, cfg.Gate, large, true)
	require.Nil(t, sum)
	require.Equal(t, large, got)
	off := cfg.Gate
	off.Enabled = false
	got, sum = gate.Apply(ctx, env, off, large, false)
	require.Nil(t, sum)
	require.Equal(t, large, got)
}

func TestGatingWithoutAStoreOrWithOneThatFails(t *testing.T) {
	cfg := toolsConfig(t)
	cfg.Gate.BudgetTokens = 100
	env := tools.Env{Cwd: t.TempDir()}
	large := seq(t, cfg, env, 1000)
	// An image beside the text is not counted and is not withheld.
	image := transcript.Part{Image: &transcript.Blob{MimeType: "image/png", Data: make([]byte, 100000)}}
	large.Parts = append(large.Parts, image)

	got, sum := gate.Apply(context.Background(), env, cfg.Gate, large, false)
	require.NotNil(t, sum)
	require.Nil(t, sum.Stored)
	require.NoError(t, sum.StoreErr)
	require.Equal(t, []transcript.Part{{Text: sum.Text()}, image}, got.Parts)

	failure := errors.New("disk full")
	env.PutBlob = func(context.Context, string, io.Reader) (transcript.Stored, error) {
		return transcript.Stored{}, failure
	}
	got, sum = gate.Apply(context.Background(), env, cfg.Gate, large, false)
	require.ErrorIs(t, sum.StoreErr, failure)
	require.Equal(t, []transcript.Part{{Text: sum.Text()}, image}, got.Parts)
}

func TestSplitTakesTheAcceptMemberOut(t *testing.T) {
	for _, c := range []struct {
		input  string
		rest   string
		accept bool
	}{
		{`{"command":"ls","accept_large_output":true}`, `{"command":"ls"}`, true},
		{`{"accept_large_output":false,"command":"ls"}`, `{"command":"ls"}`, false},
		{`{"accept_large_output":true}`, `{}`, true},
		// Untouched, byte for byte, when the member is not there.
		{`{ "command": "echo accept_large_output" }`, `{ "command": "echo accept_large_output" }`, false},
		{`{"nested":{"accept_large_output":true}}`, `{"nested":{"accept_large_output":true}}`, false},
		{`"free text accept_large_output"`, `"free text accept_large_output"`, false},
	} {
		rest, accept, err := gate.Split(jsontext.Value(c.input))
		require.NoError(t, err, c.input)
		require.Equal(t, c.rest, string(rest), c.input)
		require.Equal(t, c.accept, accept, c.input)
	}
	_, _, err := gate.Split(jsontext.Value(`{"accept_large_output":"yes"}`))
	require.ErrorIs(t, err, gate.ErrAcceptNotBoolean)
}
