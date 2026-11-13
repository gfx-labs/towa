package compact

import (
	"encoding/json/jsontext"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

func TestBoundedSummaryRequest(t *testing.T) {
	for _, prompt := range []string{config.SummaryCheckpoint, config.SummaryShort} {
		t.Run(prompt, func(t *testing.T) {
			old := strings.Repeat("earlier 記録\n", 3000)
			recent := "Most recent instruction: preserve the output file."
			entries := session(
				transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: old}}}},
				transcript.Item{Text: &transcript.Text{Text: recent}},
			)
			in := SummaryInput{Prompt: prompt, Model: "summary", MaxOutputTokens: 1000, Replaced: entries}
			const window = 6000
			req, report, err := BoundedSummaryRequest(in, window)
			require.NoError(t, err)
			require.LessOrEqual(t, report.InputTokens+report.ReservedTokens, int64(window))
			require.NotEmpty(t, report.Omissions)
			require.Positive(t, report.Omissions[len(report.Omissions)-1].Bytes)
			text := req.Items[0].User.Parts[0].Text
			require.True(t, utf8.ValidString(text))
			require.Contains(t, text, recent)
			require.Contains(t, text, "Oldest conversation text omitted")
			require.Equal(t, old, entries[0].Item.User.Parts[0].Text)
			in.Previous = strings.Repeat("previous summary ", 3000)
			_, _, err = BoundedSummaryRequest(in, window)
			require.Error(t, err)
		})
	}
}

// settings are the values Decide is tested with. They are jcode's, which
// are also the defaults in src/config/compaction.go.
func settings(mode string) config.Compaction {
	return config.Compaction{
		Mode: mode, SummaryPrompt: config.SummaryCheckpoint,
		KeepRecentMessages: 10, EmergencyThreshold: 0.95, EmergencyKeepRecentMessages: 2,
		ReactiveThreshold: 0.80, ReactiveThresholdTokens: 400_000,
		ProactiveThreshold: 0.80, LookaheadTurns: 15, EWMAAlpha: 0.3,
		ProactiveFloor: 0.40, ProactiveFloorTokens: 150_000,
		MinSamples: 3, StallWindow: 5, MinTurnsBetween: 10, HistoryWindow: 20,
		TopicShiftThreshold: 0.45, RelevanceKeepThreshold: 0.65, GoalWindowMessages: 5,
		EmbedMaxChars: 512, EmbeddingHistoryWindow: 10,
		OpenAINative: config.NativeOff, OpenAINativeThresholdTokens: 200_000,
	}
}

func TestDecide(t *testing.T) {
	const window = 200_000
	growing := []int64{70_000, 80_000, 90_000, 100_000}
	flat := []int64{99_700, 99_800, 99_900, 100_000}
	oneTopic := [][]float32{{1, 0}, {1, 0}, {1, 0}, {1, 0}}
	twoTopics := [][]float32{{1, 0}, {1, 0}, {0, 1}, {0, 1}}

	cases := []struct {
		name   string
		mode   string
		change func(*Input)
		tokens int64
		want   Action
		by     Trigger
		native bool
		keep   int
	}{
		{name: "reactive below both thresholds", mode: config.CompactionReactive, tokens: 100_000, want: None, keep: 10},
		{name: "reactive at the fraction", mode: config.CompactionReactive, tokens: 160_000, want: Background, by: TriggerFraction, keep: 10},
		{
			name: "reactive at the token count on a large window", mode: config.CompactionReactive, tokens: 400_000,
			change: func(in *Input) { in.ContextWindow = 1_000_000 },
			want:   Background, by: TriggerTokens, keep: 10,
		},
		{
			name: "max_context_tokens caps a large window", mode: config.CompactionReactive, tokens: 170_000,
			change: func(in *Input) { in.ContextWindow, in.Config.MaxContextTokens = 1_000_000, 200_000 },
			want:   Background, by: TriggerFraction, keep: 10,
		},
		{
			name: "nothing to replace beyond the kept messages", mode: config.CompactionReactive, tokens: 160_000,
			change: func(in *Input) { in.Messages = 10 },
			want:   None,
		},
		{
			name: "a compaction is already running", mode: config.CompactionReactive, tokens: 160_000,
			change: func(in *Input) { in.Running = true },
			want:   None,
		},
		{
			name: "the next request would not fit, even with one running", mode: config.CompactionReactive, tokens: 190_000,
			change: func(in *Input) { in.Running = true },
			want:   Emergency, by: TriggerFull, keep: 2,
		},
		{
			name: "would not fit and nothing can be cut", mode: config.CompactionReactive, tokens: 190_000,
			change: func(in *Input) { in.Messages = 2 },
			want:   Stop, by: TriggerFull,
		},
		{name: "off stops when the request would not fit", mode: config.CompactionOff, tokens: 190_000, want: Stop, by: TriggerFull},
		{name: "off does nothing below that", mode: config.CompactionOff, tokens: 170_000, want: None, keep: 10},
		{
			name: "proactive projects growth past the threshold", mode: config.CompactionProactive, tokens: 100_000,
			change: func(in *Input) { in.History = growing },
			want:   Background, by: TriggerProjection, keep: 10,
		},
		{
			name: "proactive waits out the cooldown", mode: config.CompactionProactive, tokens: 100_000,
			change: func(in *Input) { in.History, in.TurnsSinceCompaction = growing, 3 },
			want:   None, keep: 10,
		},
		{
			name: "proactive ignores a stalled context", mode: config.CompactionProactive, tokens: 100_000,
			change: func(in *Input) { in.History = []int64{100_000, 120_000, 110_000, 100_000, 100_000} },
			want:   None, keep: 10,
		},
		{
			name: "proactive below the floor", mode: config.CompactionProactive, tokens: 50_000,
			change: func(in *Input) { in.History = []int64{20_000, 30_000, 40_000, 50_000} },
			want:   None, keep: 10,
		},
		{
			name: "proactive with too few samples", mode: config.CompactionProactive, tokens: 100_000,
			change: func(in *Input) { in.History = growing[2:] },
			want:   None, keep: 10,
		},
		{
			name: "proactive with slow growth", mode: config.CompactionProactive, tokens: 100_000,
			change: func(in *Input) { in.History = flat },
			want:   None, keep: 10,
		},
		{
			name: "semantic on a topic shift", mode: config.CompactionSemantic, tokens: 100_000,
			change: func(in *Input) { in.History, in.Embeddings = flat, twoTopics },
			want:   Background, by: TriggerTopicShift, keep: 10,
		},
		{
			name: "semantic on one topic with slow growth", mode: config.CompactionSemantic, tokens: 100_000,
			change: func(in *Input) { in.History, in.Embeddings = flat, oneTopic },
			want:   None, keep: 10,
		},
		{
			name: "semantic while topic history is still warming", mode: config.CompactionSemantic, tokens: 100_000,
			change: func(in *Input) { in.History = growing },
			want:   Background, by: TriggerProjection, keep: 10,
		},
		{
			name: "native auto leaves it to the server", mode: config.CompactionReactive, tokens: 160_000,
			change: func(in *Input) { in.NativeAvailable, in.Config.OpenAINative = true, config.NativeAuto },
			want:   None, keep: 10,
		},
		{
			name: "native explicit at its threshold", mode: config.CompactionReactive, tokens: 210_000,
			change: func(in *Input) {
				in.NativeAvailable, in.Config.OpenAINative, in.ContextWindow = true, config.NativeExplicit, 400_000
			},
			want: Background, by: TriggerNative, native: true, keep: 10,
		},
		{
			name: "native settings do not apply to another provider", mode: config.CompactionReactive, tokens: 160_000,
			change: func(in *Input) { in.Config.OpenAINative = config.NativeAuto },
			want:   Background, by: TriggerFraction, keep: 10,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := Input{
				Config: settings(c.mode), ContextWindow: window, Messages: 30, TurnsSinceCompaction: 10,
				// The context is the input with both cache counts.
				Usage: transcript.Usage{InputTokens: c.tokens - 30_000, CacheReadTokens: 20_000, CacheWriteTokens: 10_000, OutputTokens: 5_000},
			}
			if c.change != nil {
				c.change(&in)
			}
			got, err := Decide(in)
			require.NoError(t, err)
			require.Equal(t, c.want.String(), got.Action.String())
			require.Equal(t, c.by, got.Trigger)
			require.Equal(t, c.native, got.Native)
			require.Equal(t, c.keep, got.Keep)
			require.Equal(t, c.tokens, got.ContextTokens)
		})
	}

	_, err := Decide(Input{Config: settings(config.CompactionReactive)})
	require.Error(t, err, "a model without a context window cannot be decided for")
}

// session builds entries from items, each following the one before.
func session(items ...transcript.Item) []transcript.Entry {
	var out []transcript.Entry
	parent := uuid.Nil()
	for _, item := range items {
		e := transcript.NewEntry(parent, time.Unix(0, 0), item)
		out, parent = append(out, e), e.ID
	}
	return out
}

func user(text string) transcript.Item {
	return transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}
}

func text(s string) transcript.Item { return transcript.Item{Text: &transcript.Text{Text: s}} }

func call(id, name, input string) transcript.Item {
	return transcript.Item{ToolCall: &transcript.ToolCall{ID: id, Name: name, Input: jsontext.Value(input)}}
}

func result(id, name, out string) transcript.Item {
	return transcript.Item{ToolResult: &transcript.ToolResult{
		CallID: id, Name: name, Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: out}},
	}}
}

func TestCutKeepsACallWithItsResult(t *testing.T) {
	entries := session(
		user("first"), text("one"),
		user("second"), text("reading"), call("c1", "read", `{"path":"a"}`), call("c2", "read", `{"path":"b"}`),
		result("c1", "read", "A"), result("c2", "read", "B"),
		text("done"), user("third"),
	)
	// The latest three messages are the results, "done" and "third". The
	// results' calls are in the message before, so it is kept whole.
	replace, firstKept := Cut(entries, 3)
	require.Equal(t, 3, replace)
	require.Equal(t, entries[3].ID, firstKept)

	replace, firstKept = Cut(entries, 2)
	require.Equal(t, 8, replace)
	require.Equal(t, entries[8].ID, firstKept)

	replace, _ = Cut(entries, 7)
	require.Zero(t, replace, "fewer messages than are kept")
}

func TestItemsAfterCompactions(t *testing.T) {
	entries := session(user("first"), text("one"), user("second"), text("two"), user("third"))
	first, err := Entry(entries[4].ID, time.Unix(1, 0), "summary of first", entries[2].ID, config.CompactionReactive)
	require.NoError(t, err)
	entries = append(entries, first)
	entries = append(entries, session(text("three"), user("fourth"))...)

	items, err := Items(entries, "anthropic")
	require.NoError(t, err)
	require.Len(t, items, 6)
	require.Contains(t, items[0].User.Parts[0].Text, "summary of first")
	require.Equal(t, []transcript.Item{user("second"), text("two"), user("third"), text("three"), user("fourth")}, items[1:])

	// A later compaction replaces the earlier summary too.
	before := append([]transcript.Entry(nil), entries...)
	second, err := Entry(entries[7].ID, time.Unix(2, 0), "summary of both", entries[6].ID, ModeManual)
	require.NoError(t, err)
	entries = append(entries, second)
	items, err = Items(entries, "anthropic")
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Contains(t, items[0].User.Parts[0].Text, "summary of both")
	require.NotContains(t, items[0].User.Parts[0].Text, "summary of first")
	require.Equal(t, []transcript.Item{text("three"), user("fourth")}, items[1:])
	require.Equal(t, before, entries[:len(before)], "every earlier entry is still there, unchanged")

	// A compaction that names an entry the transcript does not hold is a
	// broken session, not an empty one.
	broken, err := Entry(uuid.Nil(), time.Unix(3, 0), "summary", uuid.NewV7(), ModeManual)
	require.NoError(t, err)
	_, err = Items(append(entries, broken), "anthropic")
	require.Error(t, err)

	_, err = Entry(uuid.Nil(), time.Unix(3, 0), " \n", entries[0].ID, ModeManual)
	require.ErrorIs(t, err, ErrEmptySummary)
}

func TestNativeCompactionGoesOnlyToItsProvider(t *testing.T) {
	entries := session(user("first"), text("one"), user("second"))
	origin := transcript.Origin{Model: "model", Account: "account"}
	native := transcript.Item{Native: &transcript.Native{Kind: "compaction"}, Provider: "openai", Origin: &origin, Opaque: jsontext.Value(`{"encrypted_content":"x"}`)}
	entry, err := NativeEntry(entries[2].ID, time.Unix(1, 0), NativeResult{Items: []transcript.Item{native}}, "openai", entries[2].ID)
	require.NoError(t, err)
	entries = append(entries, entry)

	require.Equal(t, &origin, entry.Item.Origin)
	items, err := Items(entries, "openai", origin)
	require.NoError(t, err)
	require.Equal(t, []transcript.Item{native, user("second")}, items)

	_, err = Items(entries, "anthropic")
	var unreadable *UnreadableError
	require.ErrorAs(t, err, &unreadable)
	require.Equal(t, entry.ID, unreadable.Entry)
	for _, target := range []transcript.Origin{{}, {Model: "other", Account: origin.Account}, {Model: origin.Model, Account: "other"}} {
		_, err := Items(entries, "openai", target)
		require.ErrorAs(t, err, &unreadable)
	}
	_, err = Items(entries, "openai")
	require.ErrorAs(t, err, &unreadable)
}

func TestSummaryRequest(t *testing.T) {
	replaced := session(
		user("fix the parser"), text("looking"), call("c1", "read", `{"path":"p.go","limit":20}`),
		result("c1", "read", strings.Repeat("x", 3000)), text("fixed"),
		transcript.Item{Usage: &transcript.Usage{Model: "m"}},
	)
	for _, c := range []struct {
		prompt, previous string
		search           bool
		system, ending   string
	}{
		{config.SummaryCheckpoint, "", false, checkpointSystem, checkpointPrompt},
		{config.SummaryCheckpoint, "", true, checkpointSystem, checkpointPrompt + "\n\n" + searchSentence},
		{config.SummaryCheckpoint, "earlier summary", false, checkpointSystem, checkpointMergePrompt},
		{config.SummaryShort, "", false, shortSystem, shortPrompt},
		{config.SummaryShort, "earlier summary", true, shortSystem, shortPrompt + " " + searchSentence},
	} {
		req, err := SummaryRequest(SummaryInput{
			Prompt: c.prompt, Model: "m", MaxOutputTokens: 4096, Replaced: replaced,
			Previous: c.previous, ConversationSearch: c.search,
		})
		require.NoError(t, err)
		require.Empty(t, req.Tools)
		require.Equal(t, []provider.SystemBlock{{Name: "compaction", Text: c.system}}, req.System)
		require.Len(t, req.Items, 1, "the conversation is one message of text, not messages to carry on with")
		got := req.Items[0].User.Parts[0].Text
		require.True(t, strings.HasSuffix(got, c.ending), "the request ends with the prompt: %q", got[max(len(got)-200, 0):])
		require.Equal(t, c.search, strings.Contains(got, searchSentence))
		require.Equal(t, c.previous != "", strings.Contains(got, "earlier summary"))
		require.Contains(t, got, "fix the parser")
		require.Contains(t, got, "p.go")
		require.NotContains(t, got, strings.Repeat("x", 2001), "a long tool result is cut")
	}

	_, err := SummaryRequest(SummaryInput{Prompt: "long", Replaced: replaced})
	require.Error(t, err)
	_, err = SummaryRequest(SummaryInput{Prompt: config.SummaryShort})
	require.Error(t, err)
}

func TestSummaryTextRefusesWhatCannotStandForTheEntries(t *testing.T) {
	summary, err := SummaryText([]transcript.Item{{Thinking: &transcript.Thinking{Text: "hm"}}, text("## Goal\n")}, provider.StopEndTurn)
	require.NoError(t, err)
	require.Equal(t, "## Goal\n", summary)

	_, err = SummaryText([]transcript.Item{text("## Goal")}, provider.StopMaxTokens)
	require.ErrorIs(t, err, ErrSummaryCutOff)
	_, err = SummaryText([]transcript.Item{text("no")}, provider.StopRefusal)
	require.ErrorIs(t, err, ErrSummaryRefused)
	_, err = SummaryText([]transcript.Item{call("c", "read", `{}`)}, provider.StopToolUse)
	require.ErrorIs(t, err, ErrSummaryToolCall)
	_, err = SummaryText([]transcript.Item{text("  ")}, provider.StopEndTurn)
	require.ErrorIs(t, err, ErrEmptySummary)
}

// The prompts are copies. This compares them with the commits they were
// copied from, read with git, when the checkouts are named:
//
//	TOWA_PI_REPO=~/.jcode/scratch/pi-src TOWA_JCODE_REPO=~/repo/github.com/1jehuang/jcode \
//	    go test ./src/prompt/compact -run TestPromptsMatchTheirSources
func TestPromptsMatchTheirSources(t *testing.T) {
	const (
		piCommit    = "4839e41d6463d759a92ee55f4ca7c8ae57e52c9c"
		jcodeCommit = "8e11eb23ea33c7b09d43aa9a3b52fa3f0728f39e"
	)
	show := func(repo, commit, path string) string {
		out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "show", commit+":"+path).Output()
		require.NoError(t, err)
		return string(out)
	}

	t.Run("pi", func(t *testing.T) {
		repo := os.Getenv("TOWA_PI_REPO")
		if repo == "" {
			t.Skip("TOWA_PI_REPO does not name a checkout of earendil-works/pi")
		}
		compaction := show(repo, piCommit, "packages/coding-agent/src/core/compaction/compaction.ts")
		utils := show(repo, piCommit, "packages/coding-agent/src/core/compaction/utils.ts")
		messages := show(repo, piCommit, "packages/coding-agent/src/core/messages.ts")

		require.Contains(t, utils, "SUMMARIZATION_SYSTEM_PROMPT = `"+checkpointSystem+"`;")
		require.Contains(t, compaction, "const SUMMARIZATION_PROMPT = `"+checkpointPrompt+"`;")
		head, instructions, ok := strings.Cut(checkpointMergePrompt, "\n\n")
		require.True(t, ok)
		require.Contains(t, compaction, "const UPDATE_SUMMARIZATION_PROMPT = `"+head+"\n\n${UPDATE_SUMMARIZATION_INSTRUCTIONS}`;")
		require.Contains(t, compaction, "const UPDATE_SUMMARIZATION_INSTRUCTIONS = `"+instructions+"`;")
		require.Contains(t, messages, "COMPACTION_SUMMARY_PREFIX = `"+summaryPrefix+"`;")
		require.Contains(t, messages, "COMPACTION_SUMMARY_SUFFIX = `"+summarySuffix+"`;")
	})

	t.Run("jcode", func(t *testing.T) {
		repo := os.Getenv("TOWA_JCODE_REPO")
		if repo == "" {
			t.Skip("TOWA_JCODE_REPO does not name a checkout of 1jehuang/jcode")
		}
		core := show(repo, jcodeCommit, "crates/jcode-compaction-core/src/lib.rs")
		base := show(repo, jcodeCommit, "crates/jcode-base/src/compaction.rs")

		require.Contains(t, core, `SUMMARY_PROMPT: &str = r#"`+shortPrompt+" "+searchSentence+`"#;`)
		require.Contains(t, base, `"`+shortSystem+`"`)
		// The Rust string is written on two lines joined by a backslash.
		format := strings.ReplaceAll(emergencyFormat, "%d", "{}")
		at := strings.Index(format, "The conversation")
		require.Positive(t, at)
		require.Contains(t, core, `"`+format[:at]+"\\\n")
		require.Contains(t, core, format[at:]+`"`)
	})
}
